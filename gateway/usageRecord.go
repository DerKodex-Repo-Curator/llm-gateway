package gateway

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/michaelquigley/df/dl"
	"github.com/openziti/llm-gateway/providers"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const runHeader = "X-LLM-Gateway-Run"

var runGrammar = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// runID returns a single valid caller-supplied run id; invalid headers are
// treated as absent and never refuse a completion.
func runID(r *http.Request) string {
	values := r.Header.Values(runHeader)
	if len(values) != 1 || !runGrammar.MatchString(values[0]) {
		return ""
	}
	return values[0]
}

type completionResult struct {
	usage     *providers.Usage
	id        string // first completion id written toward the client
	errorType string // error type written toward the client; empty on success
}

type usageRecord struct {
	key       string
	run       string
	provider  string
	model     string
	streaming bool
	duration  time.Duration
	result    completionResult
}

type usageField struct {
	key   string
	value any
}

func (r usageRecord) fields() []usageField {
	fields := []usageField{
		{"key", r.key},
		{"run", r.run},
		{"provider", r.provider},
		{"model", r.model},
		{"streaming", r.streaming},
		{"usage_reported", r.result.usage != nil},
	}
	if u := r.result.usage; u != nil {
		fields = append(fields,
			usageField{"prompt_tokens", u.PromptTokens},
			usageField{"completion_tokens", u.CompletionTokens},
			usageField{"total_tokens", u.TotalTokens},
		)
	}
	return append(fields,
		usageField{"duration_s", r.duration.Seconds()},
		usageField{"completion_id", r.result.id},
		usageField{"error_type", r.result.errorType},
	)
}

// meterCompletion measures every dispatched completion once: counters when
// metrics are enabled, and a usage record regardless of metrics configuration.
func (g *Gateway) meterCompletion(ctx context.Context, rec usageRecord, result completionResult) {
	rec.result = result
	if g.meters != nil {
		attrs := metric.WithAttributes(
			attribute.String("provider", rec.provider),
			attribute.String("model", rec.model),
			attribute.String("streaming", strconv.FormatBool(rec.streaming)),
			attribute.String("key", rec.key),
		)
		g.meters.requests.Add(ctx, 1, attrs)
		g.meters.requestDuration.Record(ctx, rec.duration.Seconds(), metric.WithAttributes(
			attribute.String("provider", rec.provider),
			attribute.String("model", rec.model),
			attribute.String("key", rec.key),
		))
		if result.usage != nil {
			g.meters.tokensPrompt.Add(ctx, int64(result.usage.PromptTokens), attrs)
			g.meters.tokensCompletion.Add(ctx, int64(result.usage.CompletionTokens), attrs)
		}
	}

	// leave usage unconfigured: the default logger adds channel=usage, while
	// a configured channel's JSON logger would omit that filtering attribute.
	b := dl.ChannelLog("usage")
	for _, field := range rec.fields() {
		b = b.With(field.key, field.value)
	}
	b.Info("usage")
}
