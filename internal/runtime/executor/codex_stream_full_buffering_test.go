package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func codexFullBufferingConfig() *config.Config {
	return &config.Config{Codex: config.CodexConfig{StreamFullBuffering: true}}
}

func codexFullBufferPartialEvents() []string {
	events := []string{codexCreatedEvent, codexInProgressEvent, codexOutputAddedEvent,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"FAILED-PARTIAL-TEXT"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"tool_failed","call_id":"failed_call","name":"exec","status":"in_progress","input":""}}`,
	}
	for i := 0; i < 333; i++ {
		events = append(events, fmt.Sprintf(`{"type":"response.custom_tool_call_input.delta","item_id":"tool_failed","output_index":1,"delta":"FAILED-TOOL-FRAGMENT","sequence_number":%d}`, i+5))
	}
	return events
}

func TestCodexFullBufferingFailoverAfterPartialToolCall(t *testing.T) {
	for _, transport := range []string{"http", "websocket", "downstream-websocket"} {
		for _, terminal := range []struct{ name, event string }{
			{"overload", `{"type":"response.failed","response":{"status":"failed","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded."}},"sequence_number":400}`},
			{"capacity", codexCapacityEvent},
		} {
			t.Run(transport+"/"+terminal.name, func(t *testing.T) {
				cfg := codexFullBufferingConfig()
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetRetryConfig(0, 0, 0)
				ctx := context.Background()
				req, opts := codexTestRequest()
				failedEvents := append(codexFullBufferPartialEvents(), terminal.event)
				var failedServer, successServer *httptest.Server
				if transport == "http" {
					failedServer = codexSSEServer(failedEvents...)
					successServer = codexSSEServer(codexCreatedEvent, codexOutputAddedEvent, codexCompletedEventBody)
					manager.RegisterExecutor(NewCodexExecutor(cfg))
				} else {
					failedServer = codexWebsocketServerHoldingConnection(t, failedEvents...)
					successServer = codexWebsocketServer(t, codexCreatedEvent, codexOutputAddedEvent, codexCompletedEventBody)
					manager.RegisterExecutor(NewCodexWebsocketsExecutor(cfg))
					req, opts = codexWebsocketRequest()
					if transport == "downstream-websocket" {
						ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
					}
				}
				defer failedServer.Close()
				defer successServer.Close()
				for i, server := range []*httptest.Server{failedServer, successServer} {
					auth := codexTestAuth(server.URL)
					auth.ID, auth.Provider, auth.Status = fmt.Sprintf("%s-%d", t.Name(), i), "codex", cliproxyauth.StatusActive
					auth.Attributes["priority"] = fmt.Sprint(100 - i)
					registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: req.Model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(ctx), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				result, errExecute := manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				payload, streamErr := drainChunks(result)
				if streamErr != nil || !strings.Contains(payload, "hello") || !strings.Contains(payload, "response.completed") {
					t.Fatalf("expected completed fallback, payload=%q err=%v", payload, streamErr)
				}
				for _, unwanted := range []string{"FAILED-", "failed_call", "server_is_overloaded", "response.failed"} {
					if strings.Contains(payload, unwanted) {
						t.Fatalf("failed attempt leaked %q", unwanted)
					}
				}
				failedAuth, _ := manager.GetByID(t.Name() + "-0")
				if failedAuth.LastError == nil {
					t.Fatal("first credential was not attempted")
				}
			})
		}
	}
}

func TestCodexFullBufferingStopsWithoutPartialOutput(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, tc := range []struct {
			name, event string
			limit       int64
		}{
			{"request-error", codexInvalidEvent, 0},
			{"unknown-server-error", `{"type":"response.failed","response":{"status":"failed","error":{"code":"unknown_error","message":"Unclassified failure"}}}`, 0},
			{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, 0},
			{"buffer-limit", codexCompletedEventBody, 256},
			{"upstream-tool", codexOverloadEvent, 0},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				cfg := codexFullBufferingConfig()
				cfg.Codex.StreamFullBufferMaxBytes = tc.limit
				events := []string{codexCreatedEvent, codexOutputAddedEvent, `{"type":"response.output_text.delta","delta":"FAILED-PARTIAL-TEXT"}`}
				if tc.name == "upstream-tool" {
					events = append(events, `{"type":"response.output_item.added","item":{"type":"mcp_call","name":"write_external_record"}}`)
				}
				events = append(events, tc.event)
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetRetryConfig(2, 0, 0)
				req, opts := codexTestRequest()
				var server *httptest.Server
				if transport == "http" {
					server = codexSSEServer(events...)
					manager.RegisterExecutor(NewCodexExecutor(cfg))
				} else {
					server = codexWebsocketServerHoldingConnection(t, events...)
					manager.RegisterExecutor(NewCodexWebsocketsExecutor(cfg))
					req, opts = codexWebsocketRequest()
				}
				defer server.Close()
				var unexpectedAttempts atomic.Int32
				unusedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					unexpectedAttempts.Add(1)
					w.WriteHeader(http.StatusBadRequest)
				}))
				defer unusedServer.Close()
				for i, endpoint := range []string{server.URL, unusedServer.URL} {
					auth := codexTestAuth(endpoint)
					auth.ID, auth.Provider = fmt.Sprintf("%s-%d", t.Name(), i), "codex"
					auth.Attributes["priority"] = fmt.Sprint(100 - i)
					registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: req.Model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				result, errExecute := manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				if result != nil || errExecute == nil {
					t.Fatalf("expected failure with no exposed stream, result=%v err=%v", result, errExecute)
				}
				if unexpectedAttempts.Load() != 0 {
					t.Fatal("non-retryable failure rotated credentials")
				}
				if tc.name == "buffer-limit" && !strings.Contains(errExecute.Error(), "response_buffer_limit_exceeded") {
					t.Fatalf("unexpected overflow error: %v", errExecute)
				}
			})
		}
	}
}

func TestCodexFullBufferingWebsocketFailureKeepsDownstreamOpen(t *testing.T) {
	server := codexWebsocketServerHoldingConnection(t, append(codexFullBufferPartialEvents(), codexOverloadEvent)...)
	defer server.Close()
	exec := NewCodexWebsocketsExecutor(codexFullBufferingConfig())
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const sessionID = "full-buffer-session"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	req, opts := codexWebsocketRequest()
	opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}
	result, errExecute := exec.ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if result != nil || errExecute == nil {
		t.Fatalf("expected failure before delivery: %v %v", result, errExecute)
	}
	select {
	case <-disconnectCh:
		t.Fatal("buffered failover closed the downstream connection")
	default:
	}
	if !exec.getOrCreateSession(sessionID).reqMu.TryLock() {
		t.Fatal("buffered failure leaked the websocket request lock")
	}
	exec.getOrCreateSession(sessionID).reqMu.Unlock()
}

func TestCodexFullBufferingExhaustionAndCredentialBudget(t *testing.T) {
	for _, budget := range []int{0, 1} {
		t.Run(fmt.Sprintf("budget-%d", budget), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range append(codexFullBufferPartialEvents(), codexOverloadEvent) {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
			}))
			defer server.Close()
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, budget)
			manager.RegisterExecutor(NewCodexExecutor(codexFullBufferingConfig()))
			req, opts := codexTestRequest()
			for i := 0; i < 3; i++ {
				auth := codexTestAuth(server.URL)
				auth.ID, auth.Provider = fmt.Sprintf("%s-%d", t.Name(), i), "codex"
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: req.Model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			result, errExecute := manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
			if result != nil || errExecute == nil || !strings.Contains(errExecute.Error(), "server_is_overloaded") {
				t.Fatalf("expected real overload with no leaked output, got %v %v", result, errExecute)
			}
			want := int32(3)
			if budget > 0 {
				want = int32(budget)
			}
			if got := attempts.Load(); got != want {
				t.Fatalf("attempts = %d, want %d", got, want)
			}
		})
	}
}

func TestCodexFullBufferingCancellationDuringOutput(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sent := make(chan struct{})
			serverClosed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverClosed)
				if transport == "websocket" {
					conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if errUpgrade != nil {
						t.Error(errUpgrade)
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
					close(sent)
					_, _, _ = conn.ReadMessage()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				w.(http.Flusher).Flush()
				close(sent)
				<-r.Context().Done()
			}))
			defer server.Close()
			req, opts := codexTestRequest()
			execute := NewCodexExecutor(codexFullBufferingConfig()).ExecuteStream
			if transport == "websocket" {
				execute = NewCodexWebsocketsExecutor(codexFullBufferingConfig()).ExecuteStream
				req, opts = codexWebsocketRequest()
			}
			done := make(chan error, 1)
			go func() {
				result, errExecute := execute(ctx, codexTestAuth(server.URL), req, opts)
				if result != nil {
					done <- fmt.Errorf("partial output escaped before completion")
					return
				}
				done <- errExecute
			}()
			<-sent
			cancel()
			select {
			case errExecute := <-done:
				if !errors.Is(errExecute, context.Canceled) {
					t.Fatalf("cancellation error = %v", errExecute)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("buffering ignored cancellation")
			}
			select {
			case <-serverClosed:
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation left the upstream connection open")
			}
		})
	}
}
