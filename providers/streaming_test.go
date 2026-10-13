package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestOpenAICompatibleStreamUsage(t *testing.T) {
	readers := []struct {
		name string
		read func(io.ReadCloser, chan<- StreamEvent)
	}{
		{"openai", NewOpenAI("test-key", "").readSSEStream},
		{"local", NewLocal("").readSSEStream},
	}
	content := `{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`
	usage := `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`
	wantChunk := &StreamChunk{
		ID: "chatcmpl-1", Object: "chat.completion.chunk", Model: "test-model",
		Choices: []Choice{{Index: 0, Delta: &Delta{Content: "hello"}}},
	}
	wantUsage := &Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	tests := []struct {
		name string
		data []string
		want []StreamEvent
	}{
		{
			name: "trailing usage only",
			data: []string{content, `{"id":"chatcmpl-1","choices":[],"usage":` + usage + `}`, "[DONE]"},
			want: []StreamEvent{{Chunk: wantChunk}, {Usage: wantUsage}, {Done: true}},
		},
		{
			name: "usage on content",
			data: []string{strings.TrimSuffix(content, "}") + `,"usage":` + usage + `}`, "[DONE]"},
			want: []StreamEvent{{Usage: wantUsage}, {Chunk: wantChunk}, {Done: true}},
		},
		{
			name: "empty choices without usage forwarded",
			data: []string{`{"choices":[]}`, "[DONE]"},
			want: []StreamEvent{{Chunk: &StreamChunk{Choices: []Choice{}}}, {Done: true}},
		},
		{
			name: "usage absent",
			data: []string{content, "[DONE]"},
			want: []StreamEvent{{Chunk: wantChunk}, {Done: true}},
		},
		{
			name: "null usage",
			data: []string{strings.TrimSuffix(content, "}") + `,"usage":null}`, `{"choices":[],"usage":null}`, "[DONE]"},
			want: []StreamEvent{{Chunk: wantChunk}, {Chunk: &StreamChunk{Choices: []Choice{}}}, {Done: true}},
		},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					var sse strings.Builder
					for _, data := range tt.data {
						sse.WriteString("data: " + data + "\n\n")
					}
					events := make(chan StreamEvent)
					go reader.read(io.NopCloser(strings.NewReader(sse.String())), events)
					var got []StreamEvent
					for event := range events {
						got = append(got, event)
					}
					if !reflect.DeepEqual(got, tt.want) {
						t.Fatalf("events = %+v, want %+v", got, tt.want)
					}
					// exercise the client writer to check that consuming usage also
					// removes the field from the forwarded JSON.
					w := httptest.NewRecorder()
					writer := NewSSEWriter(w)
					for _, event := range got {
						if event.Chunk != nil {
							if err := writer.WriteChunk(event.Chunk); err != nil {
								t.Fatal(err)
							}
						}
					}
					if strings.Contains(w.Body.String(), `"usage"`) {
						t.Errorf("forwarded usage to client: %s", w.Body.String())
					}
				})
			}
		})
	}
}

func TestOpenAICompatibleUsageInjection(t *testing.T) {
	factories := []struct {
		name string
		new  func(string) Provider
	}{
		{"openai", func(url string) Provider { return NewOpenAI("test-key", url) }},
		{"local", func(url string) Provider { return NewLocal(url) }},
	}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			bodies := make(chan map[string]any, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				bodies <- body
				if body["stream"] == true {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
				}
			}))
			defer server.Close()
			provider := factory.new(server.URL)
			var req ChatCompletionRequest
			// a client-sent option is dropped at decode; the gateway asks for
			// usage independently. reuse the request to catch option leakage.
			if err := json.Unmarshal([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"temperature":0.5,"stream_options":{"include_usage":false}}`), &req); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"model":      "test-model",
				"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
				"max_tokens": float64(100), "temperature": 0.5,
				"stream": true, "stream_options": map[string]any{"include_usage": true},
			}
			events, err := provider.ChatCompletionStream(context.Background(), &req)
			if err != nil {
				t.Fatal(err)
			}
			var done bool
			for event := range events {
				if event.Err != nil {
					t.Fatal(event.Err)
				}
				done = done || event.Done
			}
			if !done {
				t.Error("stream did not emit Done")
			}
			if got := <-bodies; !reflect.DeepEqual(got, want) {
				t.Errorf("streaming body = %#v, want %#v", got, want)
			}
			if _, err := provider.ChatCompletion(context.Background(), &req); err != nil {
				t.Fatal(err)
			}
			delete(want, "stream")
			delete(want, "stream_options")
			if got := <-bodies; !reflect.DeepEqual(got, want) {
				t.Errorf("non-streaming body = %#v, want %#v", got, want)
			}
		})
	}
}

func TestChoiceFinishReasonJSONShape(t *testing.T) {
	// intermediate chunk: nil FinishReason must serialize as null, not vanish.
	b, _ := json.Marshal(Choice{Index: 0, Delta: &Delta{}})
	if !strings.Contains(string(b), `"finish_reason":null`) {
		t.Errorf("intermediate choice = %s, want finish_reason:null", b)
	}

	// terminal chunk: populated FinishReason serializes as its value.
	fr := "stop"
	b, _ = json.Marshal(Choice{Index: 0, Delta: &Delta{}, FinishReason: &fr})
	if !strings.Contains(string(b), `"finish_reason":"stop"`) {
		t.Errorf("terminal choice = %s, want finish_reason:stop", b)
	}
}

func TestParseStreamChunkErrorEnvelope(t *testing.T) {
	// an error envelope must surface as an error, not an all-zero chunk.
	_, err := parseStreamChunk(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`)
	if err == nil {
		t.Fatal("error envelope must return an error")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("err = %q, want the upstream message", err)
	}

	// a normal chunk parses as a chunk.
	chunk, err := parseStreamChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk"}`)
	if err != nil {
		t.Fatalf("normal chunk parse = %v, want nil", err)
	}
	if chunk == nil || chunk.ID != "chatcmpl-1" {
		t.Errorf("chunk = %+v, want id chatcmpl-1", chunk)
	}
}

func TestOpenAIStreamErrorEnvelopeSurfaces(t *testing.T) {
	o := NewOpenAI("test-key", "")

	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		``,
		`data: {"error":{"message":"upstream overloaded","type":"server_error"}}`,
		``,
	}, "\n")

	events := make(chan StreamEvent, 10)
	go o.readSSEStream(io.NopCloser(strings.NewReader(sse)), events)

	var streamErr error
	var done bool
	for ev := range events {
		if ev.Err != nil {
			streamErr = ev.Err
		}
		if ev.Done {
			done = true
		}
	}

	if streamErr == nil {
		t.Fatal("mid-stream error envelope must surface as a stream error")
	}
	if !strings.Contains(streamErr.Error(), "upstream overloaded") {
		t.Errorf("stream error = %q, want the upstream message", streamErr)
	}
	if done {
		t.Error("an errored stream must not also emit Done")
	}
}
