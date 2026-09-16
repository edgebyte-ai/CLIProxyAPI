package helps

import (
	"errors"
	"strings"
	"testing"
)

func TestCodexResponseBufferOwnsChunksAndEnforcesLimit(t *testing.T) {
	b := NewCodexResponseBuffer(132)
	chunk := []byte("ab")
	if err := b.Append([][]byte{chunk, []byte("cd")}); err != nil {
		t.Fatal(err)
	}
	chunk[0] = 'x'
	if got := string(b.Chunks()[0]); got != "ab" {
		t.Fatalf("buffer retained mutable source bytes: %q", got)
	}
	if err := b.Append([][]byte{[]byte("e")}); err == nil || !strings.Contains(err.Error(), "response_buffer_limit_exceeded") {
		t.Fatalf("expected bounded buffer error, got %v", err)
	}
	if len(b.Chunks()) != 2 {
		t.Fatal("overflow retained extra data")
	}
	if got := NewCodexResponseBuffer(0).limit; got != 64*1024*1024 {
		t.Fatalf("default limit = %d", got)
	}
}

func TestCodexResponseBufferReplayRequiresNoUpstreamToolActivity(t *testing.T) {
	for _, tc := range []struct {
		event string
		safe  bool
	}{
		{`{"type":"response.output_item.added","item":{"type":"custom_tool_call"}}`, true},
		{`{"type":"response.output_item.added","item":{"type":"function_call"}}`, true},
		{`{"type":"response.mcp_call.in_progress"}`, false},
		{`{"type":"response.output_item.done","item":{"type":"web_search_call"}}`, false},
		{`{"type":"response.failed","response":{"output":[{"type":"mcp_call"}]}}`, false},
	} {
		b := NewCodexResponseBuffer(0)
		b.ObserveEvent([]byte(tc.event))
		if b.CanReplay() != tc.safe {
			t.Fatalf("CanReplay(%s) = %t, want %t", tc.event, b.CanReplay(), tc.safe)
		}
	}
}

func TestCodexResponseBufferTerminalValidation(t *testing.T) {
	for _, tc := range []struct {
		event string
		fail  bool
	}{
		{`{"type":"response.completed","response":{"status":"completed"}}`, false},
		{`{"type":"response.done","response":{"status":"completed"}}`, false},
		{`{"type":"response.incomplete","response":{"status":"incomplete"}}`, true},
		{`{"type":"response.completed","response":{"status":"failed"}}`, true},
	} {
		if err := ValidateCodexBufferedTerminal([]byte(tc.event)); (err != nil) != tc.fail {
			t.Fatalf("Validate(%s) error = %v", tc.event, err)
		}
	}
	original := newBufferedResponseError("test", "original")
	stopped := StopStreamRetry(original)
	if !errors.Is(stopped, original) || stopped.Error() != original.Error() {
		t.Fatal("stop marker changed the original error")
	}
}
