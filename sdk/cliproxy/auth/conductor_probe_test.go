package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type codexProbeTestExecutor struct {
	models    []string
	onExecute func(*Auth) error
}

func (e *codexProbeTestExecutor) Identifier() string { return "codex" }

func (e *codexProbeTestExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.models = append(e.models, req.Model)
	if e.onExecute != nil {
		return cliproxyexecutor.Response{}, e.onExecute(auth)
	}
	return cliproxyexecutor.Response{}, nil
}

func TestProbeCodexCooldownIgnoresResultsAfterCredentialReplacement(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	for _, replacementMode := range []string{"credential rotation", "registration replacement"} {
		for _, success := range []bool{true, false} {
			outcome := "success"
			if !success {
				outcome = "failure"
			}
			t.Run(replacementMode+"/"+outcome, func(t *testing.T) {
				ctx := context.Background()
				model := "gpt-5.6-sol"
				retryAt := time.Now().Add(time.Hour)
				executor := &codexProbeTestExecutor{}
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.RegisterExecutor(executor)
				initial, errRegister := manager.Register(ctx, &Auth{
					ID: "probe-stale-" + t.Name(), Provider: "codex", Status: StatusError,
					Metadata:    map[string]any{"access_token": "old-token"},
					Unavailable: true, NextRetryAfter: retryAt,
					Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: retryAt},
					ModelStates: map[string]*ModelState{
						model: {Unavailable: true, Status: StatusError, NextRetryAfter: retryAt},
					},
				})
				if errRegister != nil {
					t.Fatalf("register auth: %v", errRegister)
				}

				var replacement *Auth
				executor.onExecute = func(probed *Auth) error {
					updated := probed.Clone()
					var errReplace error
					if replacementMode == "credential rotation" {
						updated.Metadata["access_token"] = "new-token"
						replacement, errReplace = manager.Update(ctx, updated)
					} else {
						manager.Remove(ctx, updated.ID)
						replacement, errReplace = manager.Register(ctx, updated)
					}
					if errReplace != nil {
						t.Fatalf("replace credential: %v", errReplace)
					}
					if success {
						return nil
					}
					return &Error{Code: "usage_limit_reached", Message: "late probe failure", HTTPStatus: http.StatusTooManyRequests}
				}

				report, errProbe := manager.ProbeCodexCooldown(ctx, initial.ID)
				if errProbe != nil {
					t.Fatalf("probe cooldown: %v", errProbe)
				}
				if len(report.Results) != 1 || report.Results[0].Success != success {
					t.Fatalf("probe report = %+v; want one result with success %v", report, success)
				}
				if replacementMode == "credential rotation" && replacement.CredentialVersion <= initial.CredentialVersion {
					t.Fatal("replacement did not advance credential version")
				}
				if replacementMode == "registration replacement" && (replacement.RegistrationEpoch <= initial.RegistrationEpoch || replacement.CredentialVersion != initial.CredentialVersion) {
					t.Fatal("replacement must advance registration epoch while retaining credential version")
				}
				current, ok := manager.GetByID(initial.ID)
				if !ok {
					t.Fatal("replacement credential disappeared")
				}
				if !reflect.DeepEqual(current.ModelStates, replacement.ModelStates) || !reflect.DeepEqual(current.Quota, replacement.Quota) || current.Unavailable != replacement.Unavailable || !current.NextRetryAfter.Equal(replacement.NextRetryAfter) {
					t.Fatalf("late probe changed replacement cooldowns: got %+v; want %+v", current, replacement)
				}
				if current.Generation != replacement.Generation || current.Success != replacement.Success || current.Failed != replacement.Failed {
					t.Fatal("late probe changed replacement execution counters or generation")
				}
			})
		}
	}
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
