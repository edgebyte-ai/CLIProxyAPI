package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type codexProbeTestExecutor struct {
	models []string
}

func (e *codexProbeTestExecutor) Identifier() string { return "codex" }

func (e *codexProbeTestExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.models = append(e.models, req.Model)
	return cliproxyexecutor.Response{}, nil
}

func (*codexProbeTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (*codexProbeTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*codexProbeTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (*codexProbeTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestProbeCodexCooldownProbesEveryCoolingModelAndClearsState(t *testing.T) {
	executor := &codexProbeTestExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "probe-auth",
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			"gpt-5.6-sol":   {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)},
			"gpt-5.6-luna":  {Unavailable: true, NextRetryAfter: time.Now().Add(2 * time.Hour)},
			"gpt-5.6-terra": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	report, errProbe := manager.ProbeCodexCooldown(context.Background(), auth.ID)
	if errProbe != nil {
		t.Fatalf("ProbeCodexCooldown() error = %v", errProbe)
	}
	if len(report.Results) != 2 || len(executor.models) != 2 {
		t.Fatalf("probe models = %#v, calls = %#v; want the two cooling models", report.Results, executor.models)
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("probed auth was not registered")
	}
	for model, state := range updated.ModelStates {
		if model == "gpt-5.6-terra" {
			continue
		}
		if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded {
			t.Fatalf("model %q remained cooling after successful probe: %+v", model, state)
		}
	}
}
