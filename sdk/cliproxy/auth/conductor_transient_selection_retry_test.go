package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestTransientSelectionUnavailableRetry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delay     time.Duration
		maxWait   time.Duration
		attempt   int
		status    int
		wantRetry bool
	}{
		{"within wait budget", time.Minute, 2 * time.Minute, 0, 503, true},
		{"beyond wait budget", time.Minute, 10 * time.Second, 0, 503, false},
		{"waiting disabled", time.Minute, 0, 0, 503, false},
		{"retry budget exhausted", time.Minute, 2 * time.Minute, 2, 503, false},
		{"no recovery deadline", 0, 2 * time.Minute, 0, 503, false},
		{"permanent failure", time.Minute, 2 * time.Minute, 0, 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const model = "transient-selection-model"
			m := NewManager(nil, nil, nil)
			m.SetRetryConfig(2, tc.maxWait, 0)
			m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
			next := time.Time{}
			if tc.delay > 0 {
				next = time.Now().Add(tc.delay)
			}
			a := &Auth{ID: "transient-selection", Provider: "codex", Status: StatusError,
				ModelStates: map[string]*ModelState{model: {
					Status: StatusError, Unavailable: true, NextRetryAfter: next,
					LastError: &Error{HTTPStatus: tc.status, Message: "server_is_overloaded"},
				}},
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(a.ID) })
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			_, _, _, errPick := m.pickNextMixed(context.Background(), []string{"codex"}, model, cliproxyexecutor.Options{}, nil)
			if !isAuthUnavailableError(errPick) {
				t.Fatalf("expected unavailable selection, got %v", errPick)
			}
			wait, retry := m.shouldRetryAfterError(errPick, tc.attempt, []string{"codex"}, model, tc.maxWait)
			if retry != tc.wantRetry {
				t.Fatalf("retry = %v, want %v (error %v)", retry, tc.wantRetry, errPick)
			}
			if retry && (wait <= 0 || wait > tc.delay) {
				t.Fatalf("unexpected recovery wait %v", wait)
			}
		})
	}
}

func TestWarnLogAuthUnavailableIncludesTransientCooldown(t *testing.T) {
	hook := setupTestLoggerHook(t)
	const model = "transient-log-model"
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	a := &Auth{ID: "transient-log", Provider: "codex", Status: StatusError,
		ModelStates: map[string]*ModelState{model: {
			Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Minute),
			LastError: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "server_is_overloaded"},
		}},
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	hook.Reset()
	m.warnLogAuthUnavailable(context.Background(), []string{"codex"}, model, cliproxyexecutor.Options{}, nil, &Error{Code: "auth_unavailable"})
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "1 of 1 candidate(s)") && strings.Contains(entry.Message, "server_is_overloaded") && strings.Contains(entry.Message, "remaining=") {
			return
		}
	}
	t.Fatal("missing temporary overload cooldown diagnostic")
}
