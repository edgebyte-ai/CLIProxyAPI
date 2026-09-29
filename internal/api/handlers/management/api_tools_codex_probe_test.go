package management

import (
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestShouldProbeCodexUsageStartsLongWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	auth := &coreauth.Auth{Provider: "codex", Status: coreauth.StatusActive}
	body := []byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":18000},"secondary_window":{"reset_after_seconds":604800}}}`)
	if !shouldProbeCodexUsage(auth, "https://chatgpt.com/backend-api/wham/usage", http.StatusOK, body, now) {
		t.Fatal("expected long inactive usage windows to trigger a probe")
	}
}

func TestShouldProbeCodexUsageHealthyShortWindowSkips(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	auth := &coreauth.Auth{Provider: "codex", Status: coreauth.StatusActive}
	body := []byte(`{"rate_limit":{"primary_window":{"reset_after_seconds":30},"secondary_window":{"reset_after_seconds":60}}}`)
	if shouldProbeCodexUsage(auth, "https://chatgpt.com/backend-api/wham/usage", http.StatusOK, body, now) {
		t.Fatal("did not expect a probe for a healthy started window")
	}
}

func TestShouldProbeCodexUsageErrorState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	auth := &coreauth.Auth{Provider: "codex", Status: coreauth.StatusError}
	if !shouldProbeCodexUsage(auth, "https://chatgpt.com/backend-api/wham/usage", http.StatusOK, []byte(`{}`), now) {
		t.Fatal("expected an error state to trigger a probe")
	}
}
