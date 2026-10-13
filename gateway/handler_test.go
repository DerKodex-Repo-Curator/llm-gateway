package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openziti/llm-gateway/keys"
	"github.com/openziti/llm-gateway/providers"
	"github.com/openziti/llm-gateway/routing"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func mustKeyStore(t *testing.T, entries []keys.EntryConfig) *keys.Store {
	t.Helper()
	store, err := keys.NewStore(entries)
	if err != nil {
		t.Fatalf("keys.NewStore() = %v, want nil", err)
	}
	return store
}

// stubStreamProvider replays a fixed sequence of stream events and then closes
// the channel, letting tests exercise terminal and non-terminal stream shapes.
type stubStreamProvider struct {
	events []providers.StreamEvent
	err    error
}

func (p *stubStreamProvider) ChatCompletion(_ context.Context, _ *providers.ChatCompletionRequest) (*providers.ChatCompletionResponse, error) {
	return nil, providers.ErrProviderError("not implemented")
}

func (p *stubStreamProvider) ChatCompletionStream(_ context.Context, _ *providers.ChatCompletionRequest) (<-chan providers.StreamEvent, error) {
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan providers.StreamEvent, len(p.events))
	for _, ev := range p.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *stubStreamProvider) ListModels(_ context.Context) ([]providers.Model, error) {
	return nil, nil
}

func TestHandlerFallbackErrorsAreOpenAIShaped(t *testing.T) {
	g := &Gateway{}
	handler := g.newHandler()

	tests := []struct {
		name    string
		method  string
		path    string
		status  int
		errType string
	}{
		{"unknown path", http.MethodPost, "/v1/completions", http.StatusNotFound, providers.ErrorTypeNotFound},
		{"wrong method on chat completions", http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed, providers.ErrorTypeInvalidRequest},
		{"wrong method on models", http.MethodDelete, "/v1/models", http.StatusMethodNotAllowed, providers.ErrorTypeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))
			if rr.Code != tt.status {
				t.Fatalf("%s %s returned %d, want %d", tt.method, tt.path, rr.Code, tt.status)
			}
			var resp providers.ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body is not an OpenAI-shaped error: %v; body:\n%s", err, rr.Body.String())
			}
			if resp.Error.Type != tt.errType {
				t.Errorf("error type = %q, want %q", resp.Error.Type, tt.errType)
			}
		})
	}
}

// stubChatProvider returns a canned non-streaming response.
type stubChatProvider struct {
	resp *providers.ChatCompletionResponse
	req  *providers.ChatCompletionRequest
	err  error
}

func (p *stubChatProvider) ChatCompletion(_ context.Context, req *providers.ChatCompletionRequest) (*providers.ChatCompletionResponse, error) {
	p.req = req
	return p.resp, p.err
}

func (p *stubChatProvider) ChatCompletionStream(_ context.Context, _ *providers.ChatCompletionRequest) (<-chan providers.StreamEvent, error) {
	return nil, providers.ErrProviderError("not implemented")
}

func (p *stubChatProvider) ListModels(_ context.Context) ([]providers.Model, error) {
	return nil, nil
}

func TestExplicitModelPassthroughNotRouteRestricted(t *testing.T) {
	rcfg := &routing.RoutingConfig{
		DefaultRoute: "coding",
		Heuristics: &routing.HeuristicsConfig{
			Enabled: true,
			Rules: []routing.HeuristicRule{
				{Match: routing.MatchCondition{Keywords: []string{"translate"}}, Route: "general"},
			},
		},
		Routes: []routing.RouteConfig{
			{Name: "coding", Model: "llama3"},
			{Name: "general", Model: "llama3"},
		},
	}
	sr, err := routing.NewSemanticRouter(context.Background(), rcfg, nil)
	if err != nil {
		t.Fatalf("router construction failed: %v", err)
	}

	stub := &stubChatProvider{resp: &providers.ChatCompletionResponse{ID: "chatcmpl-stub"}}
	pmap := map[providers.ProviderType]providers.Provider{providers.ProviderLocal: stub}
	g := &Gateway{
		cfg:            &Config{},
		providers:      pmap,
		router:         providers.NewRouter(pmap),
		semanticRouter: sr,
		keyStore:       mustKeyStore(t, []keys.EntryConfig{{Name: "restricted", Key: "sk-gw-test", AllowedRoutes: []string{"coding"}}}),
	}
	handler := g.newHandler()

	// an explicit model selects no semantic route; a route-restricted key must
	// not be denied for it.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-gw-test")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("explicit-model request returned %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// a semantic decision landing on a disallowed route still denies.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"translate this please"}]}`))
	req.Header.Set("Authorization", "Bearer sk-gw-test")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("disallowed semantic route returned %d, want 403; body: %s", rr.Code, rr.Body.String())
	}
}

func TestCapabilityModelAliasResolvesBeforeDispatch(t *testing.T) {
	sr, err := routing.NewSemanticRouter(context.Background(), &routing.RoutingConfig{
		Routes: []routing.RouteConfig{{Name: "frontier-coding", Model: "llama3"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubChatProvider{resp: &providers.ChatCompletionResponse{
		ID: "chatcmpl-capability", Model: "upstream-alias",
	}}
	pmap := map[providers.ProviderType]providers.Provider{providers.ProviderLocal: stub}
	g := &Gateway{
		cfg:            &Config{},
		providers:      pmap,
		router:         providers.NewRouter(pmap),
		semanticRouter: sr,
	}
	handler := g.newHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"sterling-capability:sterling-classes/v1/frontier-coding","messages":[{"role":"user","content":"hi"}]}`,
	))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("capability request returned %d: %s", rr.Code, rr.Body.String())
	}
	if stub.req == nil || stub.req.Model != "llama3" {
		t.Fatalf("provider request = %+v, want resolved concrete model", stub.req)
	}
	var response providers.ChatCompletionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "llama3" {
		t.Fatalf("reported model = %q, want gateway binding", response.Model)
	}

	allowExplicit := false
	denyExplicitRouter, err := routing.NewSemanticRouter(context.Background(), &routing.RoutingConfig{
		AllowExplicitModel: &allowExplicit,
		Routes:             []routing.RouteConfig{{Name: "frontier-coding", Model: "llama3"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.semanticRouter = denyExplicitRouter
	stub.req = nil
	handler = g.newHandler()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"sterling-capability:sterling-classes/v1/frontier-coding","messages":[{"role":"user","content":"hi"}]}`,
	))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || stub.req != nil {
		t.Fatalf("explicit-model-disabled capability returned %d, provider request %+v", rr.Code, stub.req)
	}
	g.semanticRouter = sr
	handler = g.newHandler()

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"sterling-capability:sterling-classes/v2/frontier-coding","messages":[{"role":"user","content":"hi"}]}`,
	))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown vocabulary returned %d, want 400", rr.Code)
	}

	g.keyStore = mustKeyStore(t, []keys.EntryConfig{{
		Name: "route-denied", Key: "sk-route-denied", AllowedRoutes: []string{"general"},
	}})
	handler = g.newHandler()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"sterling-capability:sterling-classes/v1/frontier-coding","messages":[{"role":"user","content":"hi"}]}`,
	))
	req.Header.Set("Authorization", "Bearer sk-route-denied")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("disallowed capability route returned %d, want 403", rr.Code)
	}

	g.keyStore = mustKeyStore(t, []keys.EntryConfig{{
		Name: "model-denied", Key: "sk-model-denied",
		AllowedRoutes: []string{"frontier-coding"}, AllowedModels: []string{"other-model"},
	}})
	handler = g.newHandler()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"sterling-capability:sterling-classes/v1/frontier-coding","messages":[{"role":"user","content":"hi"}]}`,
	))
	req.Header.Set("Authorization", "Bearer sk-model-denied")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("disallowed capability model returned %d, want 403", rr.Code)
	}
}

func TestStreamingChannelCloseWithoutTerminalEvent(t *testing.T) {
	g := &Gateway{}
	provider := &stubStreamProvider{events: []providers.StreamEvent{
		{Chunk: &providers.StreamChunk{ID: "chatcmpl-test", Object: "chat.completion.chunk"}},
	}}

	rr := httptest.NewRecorder()
	g.handleStreamingCompletion(context.Background(), rr, provider, &providers.ChatCompletionRequest{})

	body := rr.Body.String()
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream without a terminal event must not end with [DONE]; body:\n%s", body)
	}
	if !strings.Contains(body, "upstream stream ended unexpectedly") {
		t.Errorf("expected an SSE error event for the truncated stream; body:\n%s", body)
	}
}

func TestStreamingDoneEmitsDoneSentinel(t *testing.T) {
	g := &Gateway{}
	provider := &stubStreamProvider{events: []providers.StreamEvent{
		{Chunk: &providers.StreamChunk{ID: "chatcmpl-test", Object: "chat.completion.chunk"}},
		{Done: true},
	}}

	rr := httptest.NewRecorder()
	g.handleStreamingCompletion(context.Background(), rr, provider, &providers.ChatCompletionRequest{})

	body := rr.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("completed stream must end with [DONE]; body:\n%s", body)
	}
	if strings.Contains(body, "upstream stream ended unexpectedly") {
		t.Errorf("completed stream must not carry a truncation error; body:\n%s", body)
	}
}

func gatewayForCompletion(t *testing.T, provider providers.Provider, authenticated bool) *Gateway {
	t.Helper()
	pmap := map[providers.ProviderType]providers.Provider{providers.ProviderLocal: provider}
	g := &Gateway{cfg: &Config{}, providers: pmap, router: providers.NewRouter(pmap)}
	if authenticated {
		g.keyStore = mustKeyStore(t, []keys.EntryConfig{{Name: "agent", Key: "sk-gw-test"}})
		t.Cleanup(func() { g.keyStore.Close() })
	}
	return g
}

func completionRequest(streaming bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"llama3","messages":[{"role":"user","content":"hello"}],"stream":`+strconv.FormatBool(streaming)+`}`))
	r.Header.Set("Authorization", "Bearer sk-gw-test")
	return r
}

func gatewayMetricReader(t *testing.T, g *Gateway) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	var err error
	g.meters, err = newMeters(provider.Meter(meterName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("meter provider shutdown: %v", err)
		}
	})
	return reader
}

func collectGatewayMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var resource metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &resource); err != nil {
		t.Fatal(err)
	}
	result := make(map[string]metricdata.Aggregation)
	for _, scope := range resource.ScopeMetrics {
		for _, item := range scope.Metrics {
			result[item.Name] = item.Data
		}
	}
	return result
}

func usageLogs(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var records []map[string]any
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode log: %v; output: %s", err, output.String())
		}
		if record["msg"] == "usage" {
			if record["channel"] != "usage" {
				t.Errorf("usage record lacks channel=usage: %#v", record)
			}
			records = append(records, record)
		}
	}
	return records
}

func TestCompletionMetering(t *testing.T) {
	usage := &providers.Usage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28}
	for _, tt := range []struct {
		name          string
		streaming     bool
		usage         *providers.Usage
		authenticated bool
		run           string
		wantRun       string
	}{
		{"streaming usage", true, usage, true, "ci/build-4411:attempt-2", "ci/build-4411:attempt-2"},
		{"non-streaming usage", false, usage, true, "run-1", "run-1"},
		{"streaming absent", true, nil, true, "invalid run", ""},
		{"non-streaming absent", false, nil, true, "", ""},
		{"reported zeros", true, &providers.Usage{}, true, "", ""},
		{"authentication off", false, usage, false, "run-2", "run-2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := captureGatewayLogs(t, true)
			var provider providers.Provider
			if tt.streaming {
				events := []providers.StreamEvent{{Chunk: &providers.StreamChunk{
					ID: "chatcmpl-first", Model: "upstream-alias", Choices: []providers.Choice{{Delta: &providers.Delta{Content: "hello"}}},
				}}}
				if tt.usage != nil {
					events = append(events,
						providers.StreamEvent{Usage: &providers.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
						providers.StreamEvent{Usage: tt.usage},
					)
				}
				events = append(events, providers.StreamEvent{Chunk: &providers.StreamChunk{Choices: []providers.Choice{}}}, providers.StreamEvent{Done: true})
				provider = &stubStreamProvider{events: events}
			} else {
				provider = &stubChatProvider{resp: &providers.ChatCompletionResponse{ID: "chatcmpl-first", Model: "upstream-alias", Usage: tt.usage}}
			}
			g := gatewayForCompletion(t, provider, tt.authenticated)
			reader := gatewayMetricReader(t, g)
			req := completionRequest(tt.streaming)
			req.Header.Set(runHeader, tt.run)
			rr := httptest.NewRecorder()
			g.newHandler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("request returned %d: %s", rr.Code, rr.Body.String())
			}
			if tt.streaming && strings.Contains(rr.Body.String(), `"usage"`) {
				t.Errorf("stream forwarded usage: %s", rr.Body.String())
			}
			keyName := ""
			if tt.authenticated {
				keyName = "agent"
			}
			attrs := attribute.NewSet(attribute.String("provider", "local"), attribute.String("model", "llama3"),
				attribute.String("streaming", strconv.FormatBool(tt.streaming)), attribute.String("key", keyName))
			metrics := collectGatewayMetrics(t, reader)
			requests, ok := metrics["llm_gateway.requests"].(metricdata.Sum[int64])
			if !ok || len(requests.DataPoints) != 1 || requests.DataPoints[0].Value != 1 || !requests.DataPoints[0].Attributes.Equals(&attrs) {
				t.Errorf("request counter = %#v, want one request with %v", requests, attrs)
			}
			durationAttrs := attribute.NewSet(attribute.String("provider", "local"), attribute.String("model", "llama3"), attribute.String("key", keyName))
			duration, ok := metrics["llm_gateway.request.duration"].(metricdata.Histogram[float64])
			if !ok || len(duration.DataPoints) != 1 || duration.DataPoints[0].Count != 1 || !duration.DataPoints[0].Attributes.Equals(&durationAttrs) {
				t.Errorf("duration histogram = %#v, want one sample with %v", duration, durationAttrs)
			}
			for _, token := range []struct {
				name  string
				value int
			}{{"llm_gateway.tokens.prompt", 20}, {"llm_gateway.tokens.completion", 8}} {
				data, found := metrics[token.name]
				if tt.usage == nil {
					if found && len(data.(metricdata.Sum[int64]).DataPoints) != 0 {
						t.Errorf("%s recorded unreported usage: %#v", token.name, data)
					}
					continue
				}
				if *tt.usage == (providers.Usage{}) {
					token.value = 0
				}
				sum, ok := data.(metricdata.Sum[int64])
				if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != int64(token.value) || !sum.DataPoints[0].Attributes.Equals(&attrs) {
					t.Errorf("%s = %#v, want %d with %v", token.name, data, token.value, attrs)
				}
			}
			records := usageLogs(t, output)
			if len(records) != 1 {
				t.Fatalf("usage record count = %d, want 1", len(records))
			}
			record := records[0]
			for name, value := range map[string]any{"key": keyName, "run": tt.wantRun, "provider": "local", "model": "llama3",
				"streaming": tt.streaming, "usage_reported": tt.usage != nil, "completion_id": "chatcmpl-first", "error_type": ""} {
				if record[name] != value {
					t.Errorf("record[%s] = %v, want %v", name, record[name], value)
				}
			}
			assertRecordTokens(t, record, tt.usage)
		})
	}
}

func assertRecordTokens(t *testing.T, record map[string]any, usage *providers.Usage) {
	t.Helper()
	values := map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if usage != nil {
		values["prompt_tokens"], values["completion_tokens"], values["total_tokens"] = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
	}
	for name, want := range values {
		got, found := record[name]
		if usage == nil {
			if found {
				t.Errorf("unmeasured record contains %s=%v", name, got)
			}
		} else if !found || got != float64(want) {
			t.Errorf("record[%s] = %v, want %d", name, got, want)
		}
	}
}

func TestRunID(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []string
		want   string
	}{
		{"valid", []string{"ci/build-4411:attempt-2_A.b"}, "ci/build-4411:attempt-2_A.b"},
		{"maximum length", []string{strings.Repeat("a", 128)}, strings.Repeat("a", 128)},
		{"missing", nil, ""}, {"empty", []string{""}, ""},
		{"too long", []string{strings.Repeat("a", 129)}, ""},
		{"whitespace", []string{"run 1"}, ""}, {"quote", []string{`run"1`}, ""},
		{"equals", []string{"run=1"}, ""}, {"control", []string{"run\n1"}, ""},
		{"unicode", []string{"rún"}, ""}, {"duplicate", []string{"run-1", "run-1"}, ""},
		{"folded duplicate", []string{"run-1,run-2"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			for _, value := range tt.values {
				r.Header.Add("x-llm-gateway-run", value)
			}
			if got := runID(r); got != tt.want {
				t.Errorf("runID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUsageRecordFields(t *testing.T) {
	for _, measured := range []bool{false, true} {
		t.Run(strconv.FormatBool(measured), func(t *testing.T) {
			r := usageRecord{key: "agent", run: "run-1", provider: "local", model: "llama3", streaming: true, duration: 1500 * time.Millisecond,
				result: completionResult{id: "chatcmpl-1", errorType: providers.ErrorTypeServer}}
			want := []usageField{{"key", "agent"}, {"run", "run-1"}, {"provider", "local"}, {"model", "llama3"}, {"streaming", true}, {"usage_reported", measured}}
			if measured {
				r.result.usage = &providers.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
				want = append(want, usageField{"prompt_tokens", 10}, usageField{"completion_tokens", 5}, usageField{"total_tokens", 15})
			}
			want = append(want, usageField{"duration_s", 1.5}, usageField{"completion_id", "chatcmpl-1"}, usageField{"error_type", providers.ErrorTypeServer})
			if got := r.fields(); !reflect.DeepEqual(got, want) {
				t.Errorf("fields = %#v, want %#v", got, want)
			}
		})
	}
}

func TestStreamingCompletionResult(t *testing.T) {
	latest := &providers.Usage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28}
	for _, measured := range []bool{false, true} {
		t.Run(strconv.FormatBool(measured), func(t *testing.T) {
			events := []providers.StreamEvent{{Chunk: &providers.StreamChunk{ID: "first"}}}
			var wantUsage *providers.Usage
			if measured {
				events = append(events, providers.StreamEvent{Usage: &providers.Usage{PromptTokens: 10}}, providers.StreamEvent{Usage: latest})
				wantUsage = latest
			}
			events = append(events, providers.StreamEvent{Chunk: &providers.StreamChunk{}}, providers.StreamEvent{Chunk: &providers.StreamChunk{ID: "later"}}, providers.StreamEvent{Done: true})
			g := &Gateway{}
			result := g.handleStreamingCompletion(t.Context(), httptest.NewRecorder(), &stubStreamProvider{events: events}, &providers.ChatCompletionRequest{Model: "llama3"})
			if result.usage != wantUsage || result.id != "first" || result.errorType != "" {
				t.Errorf("result = %+v, want latest usage, first id, and no error", result)
			}
		})
	}
}

func TestUsageRecordWithoutMetrics(t *testing.T) {
	for _, usage := range []*providers.Usage{nil, {PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}} {
		t.Run(strconv.FormatBool(usage != nil), func(t *testing.T) {
			output := captureGatewayLogs(t, true)
			g := gatewayForCompletion(t, &stubChatProvider{resp: &providers.ChatCompletionResponse{ID: "chatcmpl-1", Usage: usage}}, true)
			r := completionRequest(false)
			r.Header.Set(runHeader, "run-1")
			rr := httptest.NewRecorder()
			g.newHandler().ServeHTTP(rr, r)
			if rr.Code != http.StatusOK || g.meters != nil {
				t.Fatalf("status = %d, meters = %v", rr.Code, g.meters)
			}
			records := usageLogs(t, output)
			if len(records) != 1 || records[0]["run"] != "run-1" || records[0]["usage_reported"] != (usage != nil) {
				t.Fatalf("usage records = %#v, want one record with run and reported presence", records)
			}
			assertRecordTokens(t, records[0], usage)
		})
	}
}

func TestCompletionErrorAttribution(t *testing.T) {
	usage := &providers.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	for _, tt := range []struct {
		name      string
		streaming bool
		provider  providers.Provider
		wantType  string
		wantLabel string
		wantUsage *providers.Usage
		wantID    string
	}{
		{name: "API error", provider: &stubChatProvider{err: providers.NewAPIError("limited", providers.ErrorTypeRateLimit)}, wantType: providers.ErrorTypeRateLimit, wantLabel: providers.ErrorTypeRateLimit},
		{name: "untyped upstream envelope", wantType: providers.ErrorTypeServer, wantLabel: providers.ErrorTypeServer},
		{name: "untyped upstream envelope before stream", streaming: true, wantType: providers.ErrorTypeServer, wantLabel: providers.ErrorTypeServer},
		{name: "non-API error", provider: &stubChatProvider{err: errors.New("offline")}, wantType: providers.ErrorTypeServer, wantLabel: "unknown"},
		{name: "refused stream", streaming: true, provider: &stubStreamProvider{err: providers.NewAPIError("limited", providers.ErrorTypeRateLimit)}, wantType: providers.ErrorTypeRateLimit, wantLabel: providers.ErrorTypeRateLimit},
		{name: "mid-stream error", streaming: true, provider: &stubStreamProvider{events: []providers.StreamEvent{
			{Chunk: &providers.StreamChunk{ID: "chatcmpl-1"}}, {Usage: usage}, {Err: providers.NewAPIError("limited", providers.ErrorTypeRateLimit)},
		}}, wantType: providers.ErrorTypeServer, wantUsage: usage, wantID: "chatcmpl-1"},
		{name: "truncation", streaming: true, provider: &stubStreamProvider{events: []providers.StreamEvent{
			{Chunk: &providers.StreamChunk{ID: "chatcmpl-1"}}, {Usage: usage},
		}}, wantType: providers.ErrorTypeServer, wantUsage: usage, wantID: "chatcmpl-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := captureGatewayLogs(t, true)
			provider := tt.provider
			if provider == nil {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if len(r.Header.Values(runHeader)) != 0 {
						t.Error("run header reached upstream")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					io.WriteString(w, `{"error":{"message":"upstream overloaded"}}`)
				}))
				defer server.Close()
				provider = providers.NewLocal(server.URL)
			}
			g := gatewayForCompletion(t, provider, true)
			reader := gatewayMetricReader(t, g)
			rr := httptest.NewRecorder()
			r := completionRequest(tt.streaming)
			r.Header.Set(runHeader, "run-1")
			g.newHandler().ServeHTTP(rr, r)
			var clientError providers.ErrorResponse
			if strings.HasPrefix(rr.Header().Get("Content-Type"), "text/event-stream") {
				for _, line := range strings.Split(rr.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var envelope providers.ErrorResponse
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.Error.Message != "" {
						clientError = envelope
					}
				}
			} else if err := json.Unmarshal(rr.Body.Bytes(), &clientError); err != nil {
				t.Fatal(err)
			}
			if clientError.Error.Type != tt.wantType {
				t.Errorf("client error type = %q, want %q; body: %s", clientError.Error.Type, tt.wantType, rr.Body.String())
			}
			records := usageLogs(t, output)
			if len(records) != 1 {
				t.Fatalf("record count = %d, want 1", len(records))
			}
			if records[0]["error_type"] != tt.wantType || records[0]["completion_id"] != tt.wantID || records[0]["usage_reported"] != (tt.wantUsage != nil) {
				t.Errorf("record = %#v, want error %s, id %q and usage presence", records[0], tt.wantType, tt.wantID)
			}
			assertRecordTokens(t, records[0], tt.wantUsage)
			if tt.wantLabel != "" {
				data := collectGatewayMetrics(t, reader)["llm_gateway.provider.errors"]
				counter, ok := data.(metricdata.Sum[int64])
				attrs := attribute.NewSet(attribute.String("error_type", tt.wantLabel))
				if !ok || len(counter.DataPoints) != 1 || counter.DataPoints[0].Value != 1 || !counter.DataPoints[0].Attributes.Equals(&attrs) {
					t.Errorf("provider error counter = %#v, want one with %v", data, attrs)
				}
			}
		})
	}
}

func TestPreDispatchRejectionsHaveNoUsageRecord(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		key  keys.EntryConfig
	}{
		{"malformed", `{`, keys.EntryConfig{Name: "agent", Key: "sk-gw-test"}},
		{"messages absent", `{"model":"llama3"}`, keys.EntryConfig{Name: "agent", Key: "sk-gw-test"}},
		{"model denied", `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`, keys.EntryConfig{Name: "agent", Key: "sk-gw-test", AllowedModels: []string{"other"}}},
		{"unroutable", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`, keys.EntryConfig{Name: "agent", Key: "sk-gw-test"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := captureGatewayLogs(t, true)
			stub := &stubChatProvider{resp: &providers.ChatCompletionResponse{ID: "unexpected"}}
			g := gatewayForCompletion(t, stub, false)
			g.keyStore = mustKeyStore(t, []keys.EntryConfig{tt.key})
			t.Cleanup(func() { g.keyStore.Close() })
			reader := gatewayMetricReader(t, g)
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			r.Header.Set("Authorization", "Bearer sk-gw-test")
			rr := httptest.NewRecorder()
			g.newHandler().ServeHTTP(rr, r)
			if rr.Code < 400 || stub.req != nil {
				t.Fatalf("expected rejection before dispatch: status=%d, provider request=%+v", rr.Code, stub.req)
			}
			if records := usageLogs(t, output); len(records) != 0 {
				t.Errorf("rejected request emitted usage: %#v", records)
			}
			if data, found := collectGatewayMetrics(t, reader)["llm_gateway.requests"]; found && len(data.(metricdata.Sum[int64]).DataPoints) != 0 {
				t.Errorf("rejected request counted as dispatched: %#v", data)
			}
		})
	}
}
