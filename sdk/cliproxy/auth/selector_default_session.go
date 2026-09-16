package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	defaultSessionIDPrefix                    = "default-session:"
	defaultSessionAffinityCacheKeyMetadataKey = "fill_first_default_session_cache_key"
	defaultSessionProvidersMetadataKey        = "fill_first_default_session_providers"
)

// defaultSessionID groups sessionless fill-first requests without merging their
// conversation identities. A missing caller scope retains the existing fallback
// instead of sharing a default binding between unidentified callers.
func (s *SessionAffinitySelector) defaultSessionID(provider, model string, metadata map[string]any) string {
	if _, fillFirst := s.fallback.(*FillFirstSelector); !fillFirst {
		return ""
	}
	callerScope := sessionMetadataString(metadata, cliproxyexecutor.CallerScopeMetadataKey)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if callerScope == "" || provider == "" {
		return ""
	}
	providers := []string{provider}
	if provider == "mixed" {
		if group, ok := metadata[defaultSessionProvidersMetadataKey].([]string); ok && len(group) > 0 {
			providers = group
		}
	}
	identity, _ := json.Marshal(struct {
		Caller    string
		Providers []string
		Model     string
	}{callerScope, providers, canonicalModelKey(model)})
	sum := sha256.Sum256(identity)
	return defaultSessionIDPrefix + hex.EncodeToString(sum[:])
}

// setDefaultSessionProviders retains the entire requested provider pool so
// credential exclusions and cooldowns cannot change the default identity mid-retry.
func (s *SessionAffinitySelector) setDefaultSessionProviders(metadata map[string]any, providerSet map[string]struct{}) {
	if _, fillFirst := s.fallback.(*FillFirstSelector); !fillFirst {
		return
	}
	providers := make([]string, 0, len(providerSet))
	for provider := range providerSet {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	metadata[defaultSessionProvidersMetadataKey] = providers
}
