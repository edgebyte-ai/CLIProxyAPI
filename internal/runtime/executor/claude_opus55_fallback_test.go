package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeExecutor_Opus55FallbackPayloadBarrier(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "claude-opus-5-5", Protocol: "claude"}}
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			fallback string
			payload  config.PayloadConfig
			want     string
		}{
			{name: "no implicit fallback"},
			{name: "explicit caller fallback", fallback: `[{"model":"caller-model"}]`, want: `[{"model":"caller-model"}]`},
			{name: "explicit empty fallback", fallback: `[]`, want: `[]`},
			{
				name:     "configured fallback wins",
				fallback: `[{"model":"caller-model"}]`,
				payload: config.PayloadConfig{OverrideRaw: []config.PayloadRule{{
					Models: models, Params: map[string]any{"fallbacks": `[{"model":"configured-model"}]`},
				}}},
				want: `[{"model":"configured-model"}]`,
			},
			{
				name:     "filtered fallback stays absent",
				fallback: `[{"model":"claude-opus-4-8"}]`,
				payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{
					Models: []config.PayloadModelRule{{Name: "claude-opus-5-5", Protocol: "claude", Match: []map[string]any{{"fallbacks.0.model": "claude-opus-4-8", "fallbacks.#": 1}}}},
					Params: []string{"fallbacks"},
				}}},
			},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var seenBody []byte
				var seenHeaders http.Header
				transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					var errRead error
					seenBody, errRead = io.ReadAll(req.Body)
					if errRead != nil {
						return nil, errRead
					}
					seenHeaders = req.Header.Clone()
					contentType := "application/json"
					body := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
					if stream {
						contentType = "text/event-stream"
						body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
				body := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"test"}]`
				if tc.fallback != "" {
					body += `,"fallbacks":` + tc.fallback
				}
				body += `}`
				auth := &cliproxyauth.Auth{ID: "opus55-fallback", Metadata: claudeOAuthTestMetadata(), Attributes: map[string]string{"api_key": "sk-ant-oat-opus55-fallback"}}
				executor := NewClaudeExecutor(&config.Config{Payload: tc.payload})
				request := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(body)}
				options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				if stream {
					result, errStream := executor.ExecuteStream(ctx, auth, request, options)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
					t.Fatal(errExecute)
				}
				if got := gjson.GetBytes(seenBody, "fallbacks").Raw; got != tc.want {
					t.Fatalf("fallbacks = %s, want %s", got, tc.want)
				}
				if tc.want == "" {
					for _, beta := range []string{claudeServerSideFallbackBeta, claudeFallbackCreditBeta} {
						if strings.Contains(seenHeaders.Get("Anthropic-Beta"), beta) {
							t.Errorf("absent fallback unexpectedly enabled %s", beta)
						}
					}
				}
			})
		}
	}
}
