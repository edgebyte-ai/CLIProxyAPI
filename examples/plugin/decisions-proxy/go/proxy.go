package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginID         = "decisions-proxy"
	decisionsPath    = "/v1/decisions"
	defaultEndpoint  = "https://api.openai.com/v1/decisions"
	maxResponseBytes = 16 << 20
)

type config struct {
	Endpoint string `yaml:"upstream-url"`
	Key      string `yaml:"api-key"`
	KeyEnv   string `yaml:"api-key-env"`
}

type decisionsProxy struct{ config atomic.Pointer[config] }

type hostRequest struct {
	pluginapi.HTTPRequest
	CallbackID string `json:"host_callback_id"`
}

func loadConfig(raw []byte) (*config, error) {
	cfg := config{Endpoint: defaultEndpoint}
	if errDecode := yaml.Unmarshal(raw, &cfg); errDecode != nil {
		// YAML decoder errors may contain raw configuration values.
		return nil, errors.New("invalid decisions-proxy configuration")
	}
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	u, errParse := url.Parse(cfg.Endpoint)
	if errParse != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback()))) {
		return nil, errors.New("upstream-url must be an HTTPS endpoint without user info or fragment (HTTP is allowed on loopback)")
	}
	if (cfg.Key == "") == (cfg.KeyEnv == "") {
		return nil, errors.New("set exactly one of api-key or api-key-env")
	}
	if cfg.KeyEnv != "" {
		cfg.Key = os.Getenv(cfg.KeyEnv)
	}
	if cfg.Key == "" || strings.IndexFunc(cfg.Key, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
		return nil, errors.New("configured upstream bearer key is empty or invalid")
	}
	return &cfg, nil
}

func registration() any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name: pluginID, Version: "0.1.0", Author: "CLIProxyAPI contributors",
			GitHubRepository: "https://github.com/edgebyte-ai/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "upstream-url", Type: pluginapi.ConfigFieldTypeString, Default: defaultEndpoint, Description: "Full upstream Decisions endpoint."},
				{Name: "api-key-env", Type: pluginapi.ConfigFieldTypeString, Description: "Name of the environment variable holding the upstream API key."},
				{Name: "api-key", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream bearer API key; use only when api-key-env is unset."},
			},
		},
		"capabilities": map[string]any{
			"executor": true, "executor_model_scope": pluginapi.ExecutorModelScopeStatic,
			"executor_input_formats":  []string{"decisions"},
			"executor_output_formats": []string{"decisions"},
		},
	}
}

func (p *decisionsProxy) dispatch(method string, raw []byte, do func(hostRequest) (pluginapi.HTTPResponse, error)) (any, *pluginabi.Error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, &pluginabi.Error{Code: "config_error", Message: "invalid plugin registration"}
		}
		cfg, errConfig := loadConfig(req.ConfigYAML)
		if errConfig != nil {
			return nil, &pluginabi.Error{Code: "config_error", Message: errConfig.Error()}
		}
		p.config.Store(cfg)
		return registration(), nil
	case pluginabi.MethodExecutorIdentifier:
		return map[string]string{"identifier": pluginID}, nil
	case pluginabi.MethodExecutorHTTPRequest:
		var req struct {
			Method     string
			URL        string
			Body       []byte
			CallbackID string `json:"host_callback_id"`
		}
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil || req.Method != http.MethodPost || req.URL != decisionsPath || req.CallbackID == "" {
			return nil, &pluginabi.Error{Code: "invalid_request", Message: "expected a Decisions POST from the host", HTTPStatus: http.StatusBadRequest}
		}
		cfg := p.config.Load()
		if cfg == nil {
			return nil, &pluginabi.Error{Code: "unavailable", Message: "Decisions plugin is not configured", HTTPStatus: http.StatusServiceUnavailable}
		}
		upstream, errDo := do(hostRequest{HTTPRequest: pluginapi.HTTPRequest{
			Method: http.MethodPost, URL: cfg.Endpoint, Body: req.Body,
			Headers:          http.Header{"Authorization": {"Bearer " + cfg.Key}, "Content-Type": {"application/json"}, "Accept": {"application/json"}},
			DisableRedirects: true, MaxResponseBytes: maxResponseBytes,
		}, CallbackID: req.CallbackID})
		if errDo != nil || upstream.StatusCode < 200 || upstream.StatusCode > 599 || len(upstream.Body) > maxResponseBytes {
			return nil, &pluginabi.Error{Code: "upstream_error", Message: "Decisions upstream request failed", HTTPStatus: http.StatusBadGateway}
		}
		headers := make(http.Header)
		for _, name := range []string{
			"Content-Type", "X-Request-Id", "Retry-After",
			"X-RateLimit-Limit-Requests", "X-RateLimit-Limit-Tokens",
			"X-RateLimit-Remaining-Requests", "X-RateLimit-Remaining-Tokens",
			"X-RateLimit-Reset-Requests", "X-RateLimit-Reset-Tokens",
		} {
			if v := upstream.Headers.Get(name); v != "" && !strings.Contains(v, cfg.Key) {
				headers.Set(name, v)
			}
		}
		// Some providers include the submitted key in their authentication error.
		if upstream.StatusCode >= 300 && bytes.Contains(upstream.Body, []byte(cfg.Key)) {
			upstream.Body = []byte(`{"error":{"message":"Decisions upstream rejected the request"}}`)
			headers.Set("Content-Type", "application/json")
		}
		return pluginapi.ExecutorHTTPResponse{StatusCode: upstream.StatusCode, Headers: headers, Body: upstream.Body}, nil
	default:
		return nil, &pluginabi.Error{Code: "unknown_method", Message: "unsupported Decisions plugin method"}
	}
}
