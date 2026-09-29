package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func defaultSessionTestOptions(caller, message string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: []byte(fmt.Sprintf(`{"messages":[{"role":"system","content":"Summarize the transcript."},{"role":"user","content":%q}]}`, message)),
		Metadata: map[string]any{
			cliproxyexecutor.CallerScopeMetadataKey:      cliproxysession.CallerScope(caller),
			cliproxyexecutor.DerivedSessionIDMetadataKey: "derived-" + message,
		},
	}
}

func assertDefaultSessionPick(t *testing.T, selector *SessionAffinitySelector, provider, model string, opts cliproxyexecutor.Options, auths []*Auth, want string) {
	t.Helper()
	got, errPick := selector.Pick(context.Background(), provider, model, opts, auths)
	if errPick != nil || got == nil || got.ID != want {
		t.Fatalf("Pick(%s, %s) = %v, %v; want %s", provider, model, got, errPick, want)
	}
}

func TestFillFirstDefaultSessionScopeAndConversationIsolation(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()
	high := &Auth{ID: "high", Attributes: map[string]string{"priority": "9"}}
	low := &Auth{ID: "low"}
	first := defaultSessionTestOptions("api-key-a", "part 6")
	before := CanonicalSessionID(first.Headers, first.OriginalRequest, first.Metadata)
	assertDefaultSessionPick(t, selector, "codex", "model", first, []*Auth{low}, low.ID)
	selector.OnResult(Result{AuthID: low.ID, Provider: "codex", Model: "model", Success: true, Options: first})
	if after := CanonicalSessionID(first.Headers, first.OriginalRequest, first.Metadata); after != before {
		t.Fatalf("default routing changed conversation identity: %q -> %q", before, after)
	}
	if first.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey] != nil {
		t.Fatal("default binding unexpectedly created an LCP conversation")
	}
	key := sessionMetadataString(first.Metadata, defaultSessionAffinityCacheKeyMetadataKey)
	if !strings.Contains(key, "default-session:") || strings.Contains(key, "api-key-a") {
		t.Fatalf("default cache key is missing its namespace or exposes the caller: %q", key)
	}
	for _, tc := range []struct {
		name, caller, provider, model, want string
	}{
		{"different transcript part", "api-key-a", "codex", "model", low.ID},
		{"same base model", "api-key-a", "codex", "model(high)", low.ID},
		{"different caller", "api-key-b", "codex", "model", high.ID},
		{"different provider", "api-key-a", "openai", "model", high.ID},
		{"different model", "api-key-a", "codex", "other-model", high.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := defaultSessionTestOptions(tc.caller, tc.name)
			assertDefaultSessionPick(t, selector, tc.provider, tc.model, opts, []*Auth{high, low}, tc.want)
			if got := CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata); got == before {
				t.Fatal("different requests unexpectedly share their conversation identity")
			}
		})
	}
}

func TestFillFirstDefaultSessionExplicitIdentityTakesPrecedence(t *testing.T) {
	t.Parallel()
	for _, signal := range []string{"header", "body", "prompt-cache-key", "conversation", "execution"} {
		t.Run(signal, func(t *testing.T) {
			selector := NewSessionAffinitySelector(&FillFirstSelector{})
			defer selector.Stop()
			a, b := &Auth{ID: "a"}, &Auth{ID: "b"}
			assertDefaultSessionPick(t, selector, "codex", "model", defaultSessionTestOptions("caller", "default"), []*Auth{b}, b.ID)
			options := func(id string) cliproxyexecutor.Options {
				opts := defaultSessionTestOptions("caller", "explicit")
				switch signal {
				case "header":
					opts.Headers = http.Header{"X-Session-Id": {id}}
				case "body":
					opts.OriginalRequest = []byte(fmt.Sprintf(`{"session_id":%q}`, id))
				case "prompt-cache-key":
					opts.OriginalRequest = []byte(fmt.Sprintf(`{"prompt_cache_key":%q}`, id))
				case "conversation":
					opts.OriginalRequest = []byte(fmt.Sprintf(`{"conversation":{"id":%q}}`, id))
				case "execution":
					opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = id
				}
				return opts
			}
			first := options("session-1")
			assertDefaultSessionPick(t, selector, "codex", "model", first, []*Auth{a, b}, a.ID)
			if first.Metadata[defaultSessionAffinityCacheKeyMetadataKey] != nil {
				t.Fatal("explicit identity received a default binding")
			}
			assertDefaultSessionPick(t, selector, "codex", "model", options("session-2"), []*Auth{b}, b.ID)
			assertDefaultSessionPick(t, selector, "codex", "model", options("session-1"), []*Auth{a, b}, a.ID)
			assertDefaultSessionPick(t, selector, "codex", "model", defaultSessionTestOptions("caller", "next part"), []*Auth{a, b}, b.ID)
		})
	}
}

func TestFillFirstDefaultSessionFailureAndExpiration(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()
	a, b := &Auth{ID: "a"}, &Auth{ID: "b"}
	first := defaultSessionTestOptions("caller", "first")
	assertDefaultSessionPick(t, selector, "codex", "model", first, []*Auth{a, b}, a.ID)
	key := sessionMetadataString(first.Metadata, defaultSessionAffinityCacheKeyMetadataKey)
	fail := func(authID string, opts cliproxyexecutor.Options, err *Error) {
		selector.OnResult(Result{AuthID: authID, Provider: "codex", Model: "model", Error: err, Options: opts})
	}
	overload := &Error{HTTPStatus: http.StatusServiceUnavailable}
	fail(a.ID, first, overload)
	if _, exists := selector.cache.Get(key); exists {
		t.Fatal("failed credential retained its default binding")
	}
	second := defaultSessionTestOptions("caller", "retry")
	assertDefaultSessionPick(t, selector, "codex", "model", second, []*Auth{b}, b.ID)
	selector.OnResult(Result{AuthID: b.ID, Provider: "codex", Model: "model", Success: true, Options: second})
	// A late result for the previous credential must not remove the replacement.
	fail(a.ID, first, overload)
	fail(b.ID, second, &Error{Code: ErrorCodeRequestScoped, HTTPStatus: http.StatusBadRequest})
	assertDefaultSessionPick(t, selector, "codex", "model", defaultSessionTestOptions("caller", "next part"), []*Auth{a, b}, b.ID)
	selector.cache.mu.Lock()
	entry := selector.cache.entries[key]
	entry.expiresAt = time.Now().Add(-time.Hour)
	selector.cache.entries[key] = entry
	selector.cache.mu.Unlock()
	assertDefaultSessionPick(t, selector, "codex", "model", defaultSessionTestOptions("caller", "expired"), []*Auth{a, b}, a.ID)
}

func TestFillFirstDefaultSessionDoesNotChangeOtherStrategiesOrUnknownCallers(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{"round-robin", "weighted-round-robin", "missing-caller"} {
		t.Run(strategy, func(t *testing.T) {
			var fallback Selector = &FillFirstSelector{}
			switch strategy {
			case "round-robin":
				fallback = &RoundRobinSelector{}
			case "weighted-round-robin":
				fallback = &WeightedRoundRobinSelector{}
			}
			selector := NewSessionAffinitySelector(fallback)
			defer selector.Stop()
			a, b := &Auth{ID: "a", Attributes: map[string]string{"priority": "9"}}, &Auth{ID: "b"}
			first, next := defaultSessionTestOptions("caller", "part 1"), defaultSessionTestOptions("caller", "part 2")
			if strategy == "missing-caller" {
				delete(first.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
				delete(next.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
			}
			assertDefaultSessionPick(t, selector, "codex", "model", first, []*Auth{b}, b.ID)
			assertDefaultSessionPick(t, selector, "codex", "model", next, []*Auth{a, b}, a.ID)
			if next.Metadata[defaultSessionAffinityCacheKeyMetadataKey] != nil {
				t.Fatal("unexpected default binding")
			}
			if strategy != "missing-caller" && next.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey] == nil {
				t.Fatal("existing LCP behavior was lost")
			}
		})
	}
}
