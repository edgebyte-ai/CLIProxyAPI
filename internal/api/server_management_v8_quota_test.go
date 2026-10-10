package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type managementV8QuotaProvider struct {
	request *pluginapi.QuotaFetchRequest
}

func (*managementV8QuotaProvider) Identifier() string { return "quota-service" }

func (*managementV8QuotaProvider) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"credential-provider"}}, nil
}

func (p *managementV8QuotaProvider) FetchQuota(_ context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	p.request = &req
	return pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Pro"}}, nil
}

func (*managementV8QuotaProvider) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{}, nil
}

func newManagementV8QuotaServer(t *testing.T, auth *coreauth.Auth, host *pluginhost.Host) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register quota credential: %v", errRegister)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), RemoteManagement: config.RemoteManagement{SecretKey: "test-password"}}
	handler := management.NewHandlerWithoutConfigFilePath(cfg, manager)
	handler.SetLocalPassword("test-password")
	handler.SetPluginHost(host)
	server := &Server{cfg: cfg, engine: gin.New(), mgmt: handler}
	server.managementRoutesEnabled.Store(true)
	server.registerManagementRoutes()
	return server
}

func requestManagementV8Quota(server *Server, authIndex string, authorized bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v8/management/quota/fetch", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	if authorized {
		req.Header.Set("Authorization", "Bearer test-password")
	}
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, req)
	return response
}

func TestManagementV8QuotaFetchResolvesProviderAndSharesAccessControl(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		enabled, home, authorized bool
		want                      int
	}{
		{"authorized provider alias", true, false, true, http.StatusOK},
		{"missing key", true, false, false, http.StatusUnauthorized},
		{"disabled", false, false, true, http.StatusNotFound},
		{"home", true, true, true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &managementV8QuotaProvider{}
			host := pluginhost.New()
			host.RegisterPluginForTest("billing-plugin", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: provider}})
			auth := &coreauth.Auth{ID: "quota-auth", Provider: "credential-provider", Metadata: map[string]any{"account": "quota-account"}}
			authIndex := auth.EnsureIndex()
			server := newManagementV8QuotaServer(t, auth, host)
			server.cfg.Home.Enabled = tc.home
			server.managementRoutesEnabled.Store(tc.enabled)
			response := requestManagementV8Quota(server, authIndex, tc.authorized)
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.want, response.Body.String())
			}
			if tc.want != http.StatusOK {
				if provider.request != nil {
					t.Fatal("blocked request reached the quota provider")
				}
				return
			}
			if provider.request == nil || provider.request.AuthID != auth.ID || provider.request.AuthIndex != authIndex || provider.request.Provider != auth.Provider || provider.request.Metadata["account"] != "quota-account" {
				t.Fatalf("quota provider received incorrect credential: %+v", provider.request)
			}
			var quota pluginapi.QuotaFetchResponse
			if errDecode := json.Unmarshal(response.Body.Bytes(), &quota); errDecode != nil {
				t.Fatal(errDecode)
			}
			if quota.Subscription == nil || quota.Subscription.Plan != "Pro" {
				t.Fatalf("unexpected normalized quota: %+v", quota)
			}
		})
	}
}

func TestManagementV8QuotaFetchNormalizesDeclarativeProbeWithoutPlugin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" || r.Header.Get("Authorization") != "Bearer probe-token" {
			http.Error(w, "incorrect probe request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan":"ProbePro","limits":[{"period":"monthly","remaining":75,"total":100}]}`))
	}))
	t.Cleanup(upstream.Close)
	auth := &coreauth.Auth{
		ID: "declarative-quota-auth", Provider: "probe-only",
		Metadata: map[string]any{
			"token": "probe-token",
			"quota_probe": map[string]any{
				"url": upstream.URL + "/usage", "method": "GET",
				"header": map[string]any{"Authorization": "Bearer $TOKEN$"},
				"mapping": map[string]any{
					"plan": "plan",
					"groups": []any{map[string]any{
						"display_name": "Usage", "buckets_path": "limits", "window_key": "period",
						"remaining_amount_key": "remaining", "total_amount_key": "total",
					}},
				},
			},
		},
	}
	authIndex := auth.EnsureIndex()
	server := newManagementV8QuotaServer(t, auth, nil)
	response := requestManagementV8Quota(server, authIndex, true)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var quota pluginapi.QuotaFetchResponse
	if errDecode := json.Unmarshal(response.Body.Bytes(), &quota); errDecode != nil {
		t.Fatal(errDecode)
	}
	if quota.Subscription == nil || quota.Subscription.Plan != "ProbePro" || len(quota.Groups) != 1 || len(quota.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected normalized probe response: %+v", quota)
	}
	if bucket := quota.Groups[0].Buckets[0]; bucket.Window != "monthly" || bucket.RemainingFraction != 0.75 {
		t.Fatalf("unexpected normalized probe bucket: %+v", bucket)
	}
}
