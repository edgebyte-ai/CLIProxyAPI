package helps

import "github.com/tidwall/gjson"

// IsCodexEmptyOutputStructure buffers only known empty containers. Tool calls and
// unknown event types must commit the stream to avoid replaying work with side effects.
func IsCodexEmptyOutputStructure(payload []byte) bool {
	event := gjson.ParseBytes(payload)
	emptyArray := func(value gjson.Result) bool {
		return !value.Exists() || value.IsArray() && len(value.Array()) == 0
	}
	switch event.Get("type").String() {
	case "response.output_item.added":
		item := event.Get("item")
		switch item.Get("type").String() {
		case "message":
			return item.Get("role").String() == "assistant" && emptyArray(item.Get("content"))
		case "reasoning":
			return emptyArray(item.Get("summary")) && emptyArray(item.Get("content")) && item.Get("encrypted_content").String() == ""
		}
	case "response.content_part.added", "response.reasoning_summary_part.added":
		part := event.Get("part")
		switch part.Get("type").String() {
		case "output_text", "reasoning_text", "summary_text":
			return part.Get("text").String() == "" && emptyArray(part.Get("annotations"))
		}
	}
	return false
}
