package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type decisionsTestExecutor struct {
	pluginapi.ProviderExecutor
	request func(context.Context, pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error)
}

func (e decisionsTestExecutor) Identifier() string { return decisionsPluginID }
func (e decisionsTestExecutor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return e.request(ctx, req)
}

func TestDecisionsRoute(t *testing.T) {
	host := pluginhost.New()
	server := newTestServerWithOptions(t, WithPluginHost(host))
	rule := []config.PayloadModelRule{{Name: "gpt-6-luna", Protocol: "decisions", Headers: map[string]string{"X-Use-Policy": "yes"}}}
	server.cfg.Payload = config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: rule, Params: []string{"private", "questions.0"}}}}
	const body = `{"model":"gpt-6-luna","private":"remove","questions":[{"type":"score"},{"type":"predicate"}]}`
	const answer = `{"error":{"code":"upstream_limit"}}`
	calls := 0
	var dispatchErr error
	request := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, decisionsPath+"?key=secret&url=https://override.invalid", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Cookie", "secret=value")
		req.Header.Set("X-Use-Policy", "yes")
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req.WithContext(ctx))
		return rr
	}
	if rr := request("test-key"); rr.Code != http.StatusNotFound {
		t.Fatalf("uninstalled status = %d; %s", rr.Code, rr.Body.String())
	}
	var pluginCtx context.Context
	host.RegisterPluginForTest(decisionsPluginID, pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Executor: decisionsTestExecutor{request: func(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
		calls++
		pluginCtx = ctx
		if ctx.Err() != nil || req.Method != http.MethodPost || req.URL != decisionsPath || req.AuthID != "" || len(req.StorageJSON) != 0 {
			t.Fatalf("unexpected plugin dispatch: method=%q path=%q auth=%q", req.Method, req.URL, req.AuthID)
		}
		if len(req.Headers) != 2 || req.Headers.Get("Content-Type") != "application/json" || req.Headers.Get("Accept") != "application/json" {
			t.Fatalf("unexpected outbound headers: %v", req.Headers)
		}
		if ojson.GetBytesNoCopy(req.Body, "private").Exists() || ojson.GetBytesNoCopy(req.Body, "questions.0.type").String() != "predicate" {
			t.Fatalf("wrong finalized payload: %s", req.Body)
		}
		if dispatchErr != nil {
			return pluginapi.ExecutorHTTPResponse{}, dispatchErr
		}
		return pluginapi.ExecutorHTTPResponse{StatusCode: http.StatusTooManyRequests, Headers: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"3"}, "X-Request-Id": {"upstream-request"}, "Set-Cookie": {"secret=bad"}}, Body: []byte(answer)}, nil
	}}}})
	if rr := request(""); rr.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("unauthenticated status = %d; plugin calls = %d", rr.Code, calls)
	}
	if rr := request("test-key"); rr.Code != http.StatusTooManyRequests || rr.Body.String() != answer || rr.Header().Get("Retry-After") != "3" || rr.Header().Get("X-Request-Id") != "upstream-request" || rr.Header().Get("Set-Cookie") != "" || calls != 1 {
		t.Fatalf("forwarded status = %d, body = %s, headers = %v, calls = %d", rr.Code, rr.Body.String(), rr.Header(), calls)
	}
	if pluginCtx.Err() != context.Canceled {
		t.Fatal("plugin context did not inherit client cancellation")
	}
	dispatchErr = errors.New("transport failed with SECRET")
	if rr := request("test-key"); rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), "SECRET") || calls != 2 {
		t.Fatalf("transport error status = %d; body = %s; calls = %d", rr.Code, rr.Body.String(), calls)
	}
	server.cfg.Home.Enabled = true
	// Exercise the route independently of the global Home heartbeat gate.
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = httptest.NewRequest(http.MethodPost, decisionsPath, strings.NewReader(body))
	server.decisions(c)
	if c.Writer.Status() != http.StatusNotFound || calls != 2 {
		t.Fatalf("Home status = %d; plugin calls = %d", c.Writer.Status(), calls)
	}
}
