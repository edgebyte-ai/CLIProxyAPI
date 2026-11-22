package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func encode(t *testing.T, data any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(data)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func TestDecisionsDispatch(t *testing.T) {
	const endpoint = "https://upstream.example/classifier/v1/decisions?version=2"
	const key = "upstream-owned-test-key"
	t.Setenv("DECISIONS_TEST_KEY", key)
	var p decisionsProxy
	lifecycle := encode(t, map[string][]byte{"config_yaml": []byte("upstream-url: " + endpoint + "\napi-key-env: DECISIONS_TEST_KEY\n")})
	_, errRegister := p.dispatch(pluginabi.MethodPluginRegister, lifecycle, nil)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	payload := []byte(` { "model": "example", "questions": [], "new_upstream_field": [1] } `)
	callerReq := encode(t, map[string]any{
		"Method": "POST", "URL": decisionsPath, "Body": payload, "host_callback_id": "request-scoped",
		"Headers": http.Header{"Cookie": {"client=secret"}, "Authorization": {"Bearer client-secret"}},
	})
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusTooManyRequests} {
		wantBody := []byte(`{"example":[1,2]}`)
		res, errCall := p.dispatch(pluginabi.MethodExecutorHTTPRequest, callerReq, func(req hostRequest) (pluginapi.HTTPResponse, error) {
			if req.URL != endpoint || req.Method != "POST" || req.CallbackID != "request-scoped" ||
				!bytes.Equal(req.Body, payload) || !req.DisableRedirects || req.MaxResponseBytes != maxResponseBytes {
				t.Fatal("incorrect upstream target, payload, or transport options")
			}
			if req.Headers.Get("Authorization") != "Bearer "+key || len(req.Headers) != 3 || req.Headers.Get("Cookie") != "" {
				t.Fatal("outbound headers do not isolate caller and provider credentials")
			}
			headers := make(http.Header)
			headers.Set("X-Request-Id", "upstream-123")
			headers.Set("Retry-After", "30")
			headers.Set("X-Ratelimit-Remaining-Requests", "0")
			headers.Set("Set-Cookie", "secret=provider")
			return pluginapi.HTTPResponse{StatusCode: status, Body: wantBody, Headers: headers}, nil
		})
		if errCall != nil {
			t.Fatal(errCall)
		}
		got := res.(pluginapi.ExecutorHTTPResponse)
		if got.StatusCode != status || !bytes.Equal(got.Body, wantBody) || len(got.Headers) != 3 ||
			got.Headers.Get("X-Ratelimit-Remaining-Requests") != "0" || got.Headers.Get("Set-Cookie") != "" {
			t.Fatal("status, body, or diagnostic header mismatch")
		}
	}
	_, errCall := p.dispatch(pluginabi.MethodExecutorHTTPRequest, callerReq, func(hostRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{}, errors.New("private diagnostic: " + key)
	})
	if errCall == nil || errCall.HTTPStatus != 502 || strings.Contains(errCall.Message, key) {
		t.Fatal("transport error disclosed private diagnostics or lost status")
	}
	res, errCall := p.dispatch(pluginabi.MethodExecutorHTTPRequest, callerReq, func(hostRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":"invalid key: ` + key + `"}`), Headers: http.Header{"X-Request-Id": {key}}}, nil
	})
	if errCall != nil {
		t.Fatal(errCall)
	}
	got := res.(pluginapi.ExecutorHTTPResponse)
	if bytes.Contains(encode(t, got.Headers), []byte(key)) || bytes.Contains(got.Body, []byte(key)) || !json.Valid(got.Body) || got.StatusCode != 401 {
		t.Fatal("reflected upstream key not suppressed")
	}
}

func TestDispatchCannotSelectTarget(t *testing.T) {
	for _, url := range []string{"https://other.example/v1/decisions", "/v1/decisions?url=https://other.example", "/v1/responses"} {
		var p decisionsProxy
		_, errCall := p.dispatch(pluginabi.MethodExecutorHTTPRequest, encode(t, map[string]string{"Method": "POST", "URL": url, "host_callback_id": "x"}), nil)
		if errCall == nil || errCall.HTTPStatus != 400 {
			t.Fatalf("unexpected result for %s", url)
		}
	}
}

func TestEndpointAndCredentialValidation(t *testing.T) {
	t.Setenv("DECISIONS_TEST_MISSING_KEY", "")
	for _, yaml := range []string{
		"upstream-url: http://nonloopback.example/test\napi-key: key",
		"upstream-url: https://user@upstream.example/test\napi-key: key",
		"upstream-url: 'https://upstream.example/test#fragment'\napi-key: key",
		"upstream-url: /relative\napi-key: key",
		"api-key: key\napi-key-env: KEY",
		"api-key-env: DECISIONS_TEST_MISSING_KEY",
		`api-key: "invalid\r\nheader"`,
		"enabled: true",
	} {
		if _, errConfig := loadConfig([]byte(yaml)); errConfig == nil {
			t.Errorf("accepted unexpected configuration %q", yaml)
		}
	}
	cfg, errConfig := loadConfig([]byte("api-key: key"))
	if errConfig != nil || cfg.Endpoint != defaultEndpoint || cfg.Key != "key" {
		t.Fatal("default endpoint and inline key rejected")
	}
	if _, errConfig := loadConfig([]byte("upstream-url: 'http://[::1]:8000/native'\napi-key: key")); errConfig != nil {
		t.Fatal(errConfig)
	}
}
