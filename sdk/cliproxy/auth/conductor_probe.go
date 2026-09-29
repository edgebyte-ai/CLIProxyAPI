package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// CodexCooldownProbeResult is the redacted result of one upstream model probe.
type CodexCooldownProbeResult struct {
	Model          string    `json:"model"`
	Success        bool      `json:"success"`
	Classification string    `json:"classification,omitempty"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	RetryAt        time.Time `json:"retry_at,omitempty"`
	Error          string    `json:"error,omitempty"`
}

// CodexCooldownProbeReport describes a manual, account-pinned cooldown refresh.
type CodexCooldownProbeReport struct {
	AuthID  string                     `json:"auth_id"`
	Results []CodexCooldownProbeResult `json:"results"`
}

// ProbeCodexCooldown sends one minimal generation request for each currently
// cooling model. When no model cooldown exists, it probes one registered model.
// It deliberately bypasses selection so another credential cannot hide the result.
func (m *Manager) ProbeCodexCooldown(ctx context.Context, authID string) (CodexCooldownProbeReport, error) {
	if m == nil {
		return CodexCooldownProbeReport{}, fmt.Errorf("auth manager is nil")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return CodexCooldownProbeReport{}, fmt.Errorf("auth id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lockValue, _ := codexProbeLocks.LoadOrStore(authID, &probeLock{})
	lock, ok := lockValue.(*probeLock)
	if !ok || lock == nil {
		return CodexCooldownProbeReport{}, fmt.Errorf("codex probe lock unavailable")
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()

	m.mu.RLock()
	auth := m.auths[authID]
	executor := ProviderExecutor(nil)
	if auth != nil {
		executor = m.executors[executorKeyFromAuth(auth)]
	}
	var authSnapshot *Auth
	if auth != nil {
		authSnapshot = auth.Clone()
	}
	m.mu.RUnlock()
	if authSnapshot == nil {
		return CodexCooldownProbeReport{}, fmt.Errorf("auth not found")
	}
	if !strings.EqualFold(strings.TrimSpace(authSnapshot.Provider), "codex") {
		return CodexCooldownProbeReport{}, fmt.Errorf("credential provider is not codex")
	}
	if executor == nil {
		return CodexCooldownProbeReport{}, fmt.Errorf("codex executor is not registered")
	}

	models := probeModels(m, authSnapshot)
	if len(models) == 0 {
		return CodexCooldownProbeReport{}, fmt.Errorf("no registered model available for probe")
	}
	report := CodexCooldownProbeReport{AuthID: authID, Results: make([]CodexCooldownProbeResult, 0, len(models))}
	successful := make([]string, 0, len(models))
	for _, routeModel := range models {
		upstreamModel := m.ResolveExecutionModel(authSnapshot, routeModel)
		if strings.TrimSpace(upstreamModel) == "" {
			upstreamModel = routeModel
		}
		payload, errPayload := json.Marshal(map[string]any{
			"model":             upstreamModel,
			"input":             "ping",
			"max_output_tokens": 1,
			"store":             false,
		})
		if errPayload != nil {
			return report, fmt.Errorf("build probe request for %s: %w", routeModel, errPayload)
		}
		req := cliproxyexecutor.Request{Model: upstreamModel, Payload: payload, Format: sdktranslator.FormatOpenAIResponse}
		opts := cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatOpenAIResponse,
			ResponseFormat: sdktranslator.FormatOpenAIResponse,
			Metadata:       map[string]any{cliproxyexecutor.RequestedModelMetadataKey: routeModel},
		}
		resp, errExecute := executor.Execute(ctx, authSnapshot, req, opts)
		_ = resp
		result := Result{
			AuthID: authID, Provider: authSnapshot.Provider, Model: routeModel,
			RouteModel: routeModel, Success: errExecute == nil, Options: opts,
		}
		probeResult := CodexCooldownProbeResult{Model: routeModel, Success: errExecute == nil}
		if errExecute == nil {
			probeResult.Classification = "success"
			successful = append(successful, routeModel)
		} else {
			result.Error = resultErrorFromError(errExecute)
			result.RetryAfter = retryAfterFromError(errExecute)
			result.CredentialScope = isCredentialScopedError(errExecute)
			probeResult.Classification = classifyCodexProbeResult(result.Error)
			probeResult.Error = probeResult.Classification
			if result.Error != nil {
				probeResult.HTTPStatus = result.Error.HTTPStatus
			}
			if result.RetryAfter != nil {
				probeResult.RetryAt = time.Now().Add(*result.RetryAfter)
			}
		}
		m.MarkResult(ctx, result)
		report.Results = append(report.Results, probeResult)
	}
	if len(successful) > 0 {
		m.clearSuccessfulProbeCooldowns(authID, successful, len(successful) == len(models))
	}
	return report, nil
}

var codexProbeLocks sync.Map

type probeLock struct{ mu sync.Mutex }

func probeModels(m *Manager, auth *Auth) []string {
	if auth == nil {
		return nil
	}
	now := time.Now()
	models := make([]string, 0)
	for key, state := range auth.ModelStates {
		if state == nil || key == "" {
			continue
		}
		active := state.NextRetryAfter.After(now) || state.Quota.NextRecoverAt.After(now)
		if active {
			models = append(models, key)
		}
	}
	if len(models) == 0 {
		registered := modelsForRegisteredAuth(auth.ID)
		if len(registered) > 0 && strings.TrimSpace(registered[0]) != "" {
			models = append(models, registered[0])
		}
	}
	sort.Strings(models)
	return models
}

func classifyCodexProbeResult(err *Error) string {
	if err == nil {
		return "success"
	}
	message := strings.ToLower(strings.TrimSpace(err.Message + " " + err.Code))
	switch {
	case strings.Contains(message, "usage_limit_reached") || strings.Contains(message, "usage limit"):
		return "usage_limit_reached"
	case strings.Contains(message, "rate_limit_error") || strings.Contains(message, "rate_limit_exceeded") || err.HTTPStatus == 429:
		return "rate_limit_error"
	case err.HTTPStatus == 503 || strings.Contains(message, "server_is_overloaded") || strings.Contains(message, "overloaded"):
		return "server_is_overloaded"
	case err.HTTPStatus == 404 || strings.Contains(message, "model_not_supported") || strings.Contains(message, "model unavailable"):
		return "model_unavailable_or_not_supported"
	default:
		return "other_error"
	}
}

func (m *Manager) clearSuccessfulProbeCooldowns(authID string, models []string, clearCredential bool) {
	now := time.Now()
	m.mu.Lock()
	auth := m.auths[authID]
	if auth == nil {
		m.mu.Unlock()
		return
	}
	for _, model := range models {
		if state := existingModelState(auth, canonicalModelKey(model)); state != nil {
			resetModelState(state, now)
		}
	}
	if clearCredential && auth.Quota.Exceeded {
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		auth.Quota = QuotaState{}
	}
	updateAggregatedAvailability(auth, now)
	if !hasModelError(auth, now) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.Generation++
	auth.UpdatedAt = now
	snapshot := auth.Clone()
	_ = m.persist(context.Background(), auth)
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if snapshot != nil {
		if models, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(authID); len(models) > 0 {
			projections := make([]registry.ClientModelProjection, 0, len(models))
			for _, model := range models {
				if model != nil && model.ID != "" {
					projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
				}
			}
			registry.GetGlobalRegistry().ApplyClientModelProjections(authID, regEpoch, snapshot.Generation, projections)
		}
	}
	if m.cooldownStore != nil {
		m.persistCooldownStates(context.Background())
	}
}
