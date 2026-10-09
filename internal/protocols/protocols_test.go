package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestNormalizeAnthropicSupportsTextToolUseAndToolResult(t *testing.T) {
	request, err := NormalizeAnthropic(map[string]any{
		"model":      "wb/model-a",
		"max_tokens": float64(32),
		"system":     []any{map[string]any{"type": "text", "text": "system"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{"q": "x"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "done"}}},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeAnthropic() error = %v", err)
	}
	if len(request.Messages) != 5 || len(request.Messages[2].ToolCalls) != 1 || request.Messages[4].Role != "tool" {
		t.Fatalf("unexpected canonical messages: %#v", request.Messages)
	}
	if request.Messages[2].ToolCalls[0].Name != "lookup" {
		t.Fatalf("unexpected tool call: %#v", request.Messages[2].ToolCalls)
	}
}

func TestNormalizeResponsesAndConversions(t *testing.T) {
	request, err := NormalizeResponses(map[string]any{
		"model":             "wb/model-a",
		"instructions":      "be concise",
		"input":             []any{map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}}},
		"max_output_tokens": float64(20),
	})
	if err != nil {
		t.Fatalf("NormalizeResponses() error = %v", err)
	}
	if len(request.Messages) != 2 || request.Messages[1].Role != "system" {
		t.Fatalf("unexpected response messages: %#v", request.Messages)
	}
	result := ports.ChatResult{ID: "chatcmpl-1", Text: "answer", CreatedAt: 1, FinishReason: "stop", Usage: &ports.Usage{PromptTokens: 2, CompletionTokens: 3}}
	anthropic := AnthropicResponse(result, request.Model)
	if anthropic["type"] != "message" || !strings.Contains(string(mustJSON(anthropic)), "answer") {
		t.Fatalf("unexpected Anthropic response: %#v", anthropic)
	}
	responses := ResponsesResponse(result, request.Model)
	if responses["object"] != "response" || !strings.Contains(string(mustJSON(responses)), "answer") {
		t.Fatalf("unexpected Responses response: %#v", responses)
	}
}

func TestProtocolNormalizersRejectUnsupportedFields(t *testing.T) {
	if _, err := NormalizeAnthropic(map[string]any{"model": "wb/model-a", "max_tokens": float64(1), "messages": []any{}, "unknown": true}); err == nil {
		t.Fatal("Anthropic normalizer accepted an unsupported field")
	}
	if _, err := NormalizeResponses(map[string]any{"model": "wb/model-a", "input": "x", "store": true}); err == nil {
		t.Fatal("Responses normalizer accepted store=true")
	}
}

func TestIncrementalProtocolSSEEventsExposeDeltaAndTerminalError(t *testing.T) {
	result := ports.ChatResult{ID: "stream-1", Text: "answer", CreatedAt: 1, FinishReason: "stop"}
	anthropicStart := AnthropicStreamStart(result, "wb/model-a")
	anthropicFinish := AnthropicStreamFinish(result, "wb/model-a")
	if len(anthropicStart) != 2 || !strings.Contains(string(anthropicStart[0]), "message_start") || !strings.Contains(string(AnthropicStreamDelta("hello")), "hello") || len(anthropicFinish) != 3 {
		t.Fatalf("unexpected Anthropic incremental events: %#v/%s/%#v", anthropicStart, AnthropicStreamDelta("hello"), anthropicFinish)
	}
	responsesStart := ResponsesStreamStart(result, "wb/model-a")
	responsesFinish := ResponsesStreamFinish(result, "wb/model-a")
	if len(responsesStart) != 4 || !strings.Contains(string(ResponsesStreamDelta(result, "hello")), "output_text.delta") || len(responsesFinish) != 4 || !strings.Contains(string(ResponsesStreamError("upstream failed")), "response.failed") {
		t.Fatalf("unexpected Responses incremental events: %#v/%s/%#v", responsesStart, ResponsesStreamDelta(result, "hello"), responsesFinish)
	}
	if strings.Contains(string(responsesFinish[0]), "response.output_text.delta") || strings.Contains(string(responsesFinish[0]), "response.content_part.added") {
		t.Fatalf("Responses finish duplicated a non-terminal event: %s", responsesFinish[0])
	}
	if !strings.Contains(string(responsesFinish[0]), "response.output_text.done") ||
		!strings.Contains(string(responsesFinish[1]), "response.content_part.done") ||
		!strings.Contains(string(responsesFinish[2]), "response.output_item.done") ||
		!strings.Contains(string(responsesFinish[3]), "response.completed") {
		t.Fatalf("unexpected Responses terminal event order: %#v", responsesFinish)
	}
	emptyFinish := ResponsesStreamFinish(ports.ChatResult{ID: "empty", CreatedAt: 1, FinishReason: "stop"}, "wb/model-a")
	if len(emptyFinish) != 4 || strings.Contains(string(emptyFinish[0]), "content_part.added") {
		t.Fatalf("empty Responses finish included setup events: %#v", emptyFinish)
	}
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
