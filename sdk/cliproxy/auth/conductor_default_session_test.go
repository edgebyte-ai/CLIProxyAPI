package auth

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagerFillFirstDefaultSessionKeepsSuccessfulFailoverAcrossRequests(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, path := range []string{"execute", "stream"} {
		t.Run(path, func(t *testing.T) {
			ctx := WithSkipPersist(context.Background())
			const model = "default-session-model"
			selector := NewSessionAffinitySelector(&FillFirstSelector{})
			defer selector.Stop()
			manager := NewManager(nil, selector, nil)
			manager.SetRetryConfig(0, 0, 0)
			highID, lowID := t.Name()+"-high", t.Name()+"-low"
			for _, auth := range []*Auth{
				{ID: highID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "9"}},
				{ID: lowID, Provider: "codex", Status: StatusActive},
			} {
				if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
					t.Fatal(errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}
			var attempts []string
			failingID := highID
			var firstKey string
			execute := func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				attempts = append(attempts, auth.ID)
				key := sessionMetadataString(opts.Metadata, defaultSessionAffinityCacheKeyMetadataKey)
				if key == "" {
					t.Error("executor did not retain the default routing key")
				}
				if firstKey == "" {
					firstKey = key
				} else if key != firstKey {
					t.Errorf("default routing key changed between attempts: %q -> %q", firstKey, key)
				}
				if id := CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata); strings.HasPrefix(id, "default-session:") {
					t.Error("default routing identity leaked into the upstream conversation identity")
				}
				if auth.ID == failingID {
					return cliproxyexecutor.Response{}, overloadStatusError()
				}
				return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
			}
			manager.RegisterExecutor(&customStreamMockExecutor{
				identifier: "codex", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: execute},
				streamFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					response, errExecute := execute(ctx, auth, req, opts)
					if errExecute != nil {
						return nil, errExecute
					}
					chunks := make(chan cliproxyexecutor.StreamChunk, 1)
					chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				},
			})
			run := func(message, want string) {
				t.Helper()
				opts := defaultSessionTestOptions("caller", message)
				req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
				var payload []byte
				if path == "execute" {
					response, errExecute := manager.Execute(ctx, []string{"codex"}, req, opts)
					if errExecute != nil {
						t.Fatal(errExecute)
					}
					payload = response.Payload
				} else {
					opts.Stream = true
					result, errStream := manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						payload = append(payload, chunk.Payload...)
					}
				}
				if string(payload) != want {
					t.Fatalf("request %q used %q, want %q", message, payload, want)
				}
			}
			run("part 6", lowID)
			expireSessionAffinityPriorityModelCooldown(t, manager, highID, model)
			failingID = ""
			run("part 7", lowID)
			failingID = lowID
			run("part 8", highID)
			if want := []string{highID, lowID, lowID, lowID, highID}; !reflect.DeepEqual(attempts, want) {
				t.Fatalf("upstream attempts = %v, want %v", attempts, want)
			}
		})
	}
}

func TestManagerFillFirstDefaultSessionProviderPoolsStayIsolated(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	const model = "default-provider-pool-model"
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	for _, provider := range []string{"provider-a", "provider-b"} {
		manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
		for _, auth := range []*Auth{
			{ID: provider + "-high", Provider: provider, Attributes: map[string]string{"priority": "9"}},
			{ID: provider + "-low", Provider: provider},
		} {
			if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		}
	}
	pick := func(providers []string, tried map[string]struct{}, want string) {
		t.Helper()
		opts := defaultSessionTestOptions("caller", "part")
		got, _, _, errPick := manager.pickNextMixed(ctx, providers, model, opts, tried)
		if errPick != nil || got == nil || got.ID != want {
			t.Fatalf("providers %v: got %v, %v; want %s", providers, got, errPick, want)
		}
	}
	pick([]string{"provider-a"}, map[string]struct{}{"provider-a-high": {}}, "provider-a-low")
	pick([]string{"provider-b"}, nil, "provider-b-high")
	pick([]string{"provider-a"}, nil, "provider-a-low")
	pick([]string{"provider-a", "provider-b"}, map[string]struct{}{"provider-a-high": {}, "provider-b-high": {}}, "provider-a-low")
	pick([]string{"provider-b", "provider-a", "provider-a"}, nil, "provider-a-low")
	pick([]string{"provider-b"}, nil, "provider-b-high")
}
