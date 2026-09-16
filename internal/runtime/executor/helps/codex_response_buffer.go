package helps

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const defaultCodexResponseBufferBytes int64 = 64 * 1024 * 1024

// CodexResponseBuffer retains one attempt's translated output before delivery.
// Its identity and contents are never shared between attempts or requests.
type CodexResponseBuffer struct {
	chunks       [][]byte
	bytes        int64
	limit        int64
	upstreamTool bool
}

func NewCodexResponseBuffer(limit int64) *CodexResponseBuffer {
	if limit <= 0 {
		limit = defaultCodexResponseBufferBytes
	}
	return &CodexResponseBuffer{limit: limit}
}

// ObserveEvent prevents replay after tools that may execute on the upstream.
// Client-executed function/custom tool calls remain safe while their output is withheld.
func (b *CodexResponseBuffer) ObserveEvent(data []byte) {
	event := gjson.GetBytes(data, "type").String()
	itemType := gjson.GetBytes(data, "item.type").String()
	if strings.HasSuffix(itemType, "_call") && itemType != "function_call" && itemType != "custom_tool_call" {
		b.upstreamTool = true
	}
	gjson.GetBytes(data, "response.output").ForEach(func(_, item gjson.Result) bool {
		typ := item.Get("type").String()
		if strings.HasSuffix(typ, "_call") && typ != "function_call" && typ != "custom_tool_call" {
			b.upstreamTool = true
		}
		return true
	})
	for _, prefix := range []string{"response.mcp_call.", "response.web_search_call.", "response.file_search_call.", "response.code_interpreter_call.", "response.image_generation_call.", "response.shell_call."} {
		if strings.HasPrefix(event, prefix) {
			b.upstreamTool = true
		}
	}
}

func (b *CodexResponseBuffer) CanReplay() bool { return !b.upstreamTool }

func (b *CodexResponseBuffer) Append(chunks [][]byte) error {
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		// Include conservative slice/channel/allocation overhead, so tiny chunks
		// cannot evade the byte limit by retaining an unbounded number of entries.
		cost := int64(len(chunk)) + 64
		if cost > b.limit-b.bytes {
			return newBufferedResponseError("response_buffer_limit_exceeded", "Codex response exceeded stream-full-buffer-max-bytes before delivery")
		}
		b.bytes += cost
		b.chunks = append(b.chunks, bytes.Clone(chunk))
	}
	return nil
}

func (b *CodexResponseBuffer) Chunks() [][]byte { return b.chunks }

// ValidateCodexBufferedTerminal rejects incomplete output without pretending it completed.
func ValidateCodexBufferedTerminal(data []byte) error {
	switch gjson.GetBytes(data, "type").String() {
	case "response.incomplete":
		return newBufferedResponseError("response_incomplete", "Codex returned an incomplete response before buffered output could be delivered")
	case "response.completed", "response.done":
		status := gjson.GetBytes(data, "response.status").String()
		if status != "" && status != "completed" {
			return newBufferedResponseError("response_incomplete", "Codex terminal response was not completed")
		}
	}
	return nil
}

type bufferedResponseError struct{ body string }

func (e *bufferedResponseError) Error() string         { return e.body }
func (e *bufferedResponseError) StatusCode() int       { return http.StatusBadGateway }
func (e *bufferedResponseError) IsRequestScoped() bool { return true }
func (e *bufferedResponseError) IsRequestStop() bool   { return true }

func newBufferedResponseError(code, message string) error {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "server_error", "code": code, "message": message}})
	return &bufferedResponseError{body: string(body)}
}

type stoppedStreamError struct{ error }

func (e stoppedStreamError) Unwrap() error       { return e.error }
func (e stoppedStreamError) IsRequestStop() bool { return true }
func (e stoppedStreamError) StatusCode() int {
	var status interface{ StatusCode() int }
	if errors.As(e.error, &status) {
		return status.StatusCode()
	}
	return http.StatusBadGateway
}

// StopStreamRetry preserves the original error and credential attribution while
// preventing a buffered terminal failure from replaying across credentials.
func StopStreamRetry(err error) error {
	if err == nil {
		return nil
	}
	return stoppedStreamError{error: err}
}
