package protocols

import (
	"fmt"
	"strings"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func NormalizeResponses(payload map[string]any) (ports.ChatInput, error) {
	allowed := map[string]bool{
		"model": true, "input": true, "instructions": true, "max_output_tokens": true,
		"temperature": true, "top_p": true, "stream": true, "metadata": true, "store": true,
	}
	for field := range payload {
		if !allowed[field] {
			return ports.ChatInput{}, &RequestError{Message: "unsupported request field: " + field}
		}
	}
	model, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return ports.ChatInput{}, &RequestError{Message: "model is required"}
	}
	stream, err := optionalBool(payload, "stream")
	if err != nil {
		return ports.ChatInput{}, err
	}
	if store, exists := payload["store"]; exists {
		value, ok := store.(bool)
		if !ok || value {
			return ports.ChatInput{}, &RequestError{Message: "store=true is not supported"}
		}
	}
	result := ports.ChatInput{Model: model, Stream: stream}
	if instructions, exists := payload["instructions"]; exists {
		text, ok := instructions.(string)
		if !ok {
			return ports.ChatInput{}, &RequestError{Message: "instructions must be a string"}
		}
		result.Messages = append(result.Messages, ports.ChatMessage{Role: "system", Content: text})
	}
	input, exists := payload["input"]
	if !exists {
		return ports.ChatInput{}, &RequestError{Message: "input must be a non-empty string or message array"}
	}
	switch value := input.(type) {
	case string:
		if value == "" {
			return ports.ChatInput{}, &RequestError{Message: "input must be a non-empty string or message array"}
		}
		result.Messages = append(result.Messages, ports.ChatMessage{Role: "user", Content: value})
	case []any:
		if len(value) == 0 {
			return ports.ChatInput{}, &RequestError{Message: "input must be a non-empty string or message array"}
		}
		for index, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				return ports.ChatInput{}, &RequestError{Message: fmt.Sprintf("input[%d] must be an object", index)}
			}
			itemType, _ := item["type"].(string)
			if itemType != "" && itemType != "message" {
				return ports.ChatInput{}, &RequestError{Message: fmt.Sprintf("input[%d] only supports message input items", index)}
			}
			role, ok := item["role"].(string)
			if !ok || (role != "system" && role != "developer" && role != "user" && role != "assistant") {
				return ports.ChatInput{}, &RequestError{Message: fmt.Sprintf("input[%d].role is not supported", index)}
			}
			content, err := responseInputText(item["content"], fmt.Sprintf("input[%d].content", index))
			if err != nil {
				return ports.ChatInput{}, err
			}
			if role == "developer" {
				role = "system"
			}
			result.Messages = append(result.Messages, ports.ChatMessage{Role: role, Content: content})
		}
	default:
		return ports.ChatInput{}, &RequestError{Message: "input must be a non-empty string or message array"}
	}
	result.MaxTokens, err = optionalPositiveInt(payload, "max_output_tokens")
	if err != nil {
		return ports.ChatInput{}, err
	}
	if result.Temperature, err = optionalNumber(payload, "temperature"); err != nil {
		return ports.ChatInput{}, err
	}
	if result.TopP, err = optionalNumber(payload, "top_p"); err != nil {
		return ports.ChatInput{}, err
	}
	if metadata, exists := payload["metadata"]; exists {
		values, ok := metadata.(map[string]any)
		if !ok {
			return ports.ChatInput{}, &RequestError{Message: "metadata must contain string values"}
		}
		for key, value := range values {
			_ = key
			if _, ok := value.(string); !ok {
				return ports.ChatInput{}, &RequestError{Message: "metadata must contain string values"}
			}
		}
	}
	return result, nil
}

func ResponsesResponse(result ports.ChatResult, model string) map[string]any {
	inputTokens, outputTokens := 0, 0
	if result.Usage != nil {
		inputTokens, outputTokens = result.Usage.PromptTokens, result.Usage.CompletionTokens
	}
	status := "completed"
	incompleteDetails := any(nil)
	if result.FinishReason == "length" {
		status = "incomplete"
		incompleteDetails = map[string]any{"reason": "max_output_tokens"}
	}
	return map[string]any{
		"id": "resp_" + result.ID, "object": "response", "created_at": result.CreatedAt,
		"status": status, "error": nil, "incomplete_details": incompleteDetails, "model": model,
		"output": []map[string]any{{
			"id": "msg_" + result.ID, "type": "message", "status": "completed", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": result.Text, "annotations": []any{}}},
		}},
		"usage": map[string]any{
			"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens,
			"input_tokens_details":  map[string]int{"cached_tokens": 0},
			"output_tokens_details": map[string]int{"reasoning_tokens": 0},
		},
	}
}

func ResponsesSSE(result ports.ChatResult, model string) [][]byte {
	response := ResponsesResponse(result, model)
	response["status"] = "in_progress"
	response["output"] = []any{}
	response["usage"] = nil
	messageID := "msg_" + result.ID
	events := [][]byte{
		SSEEvent("response.created", map[string]any{"type": "response.created", "response": response}),
		SSEEvent("response.in_progress", map[string]any{"type": "response.in_progress", "response": response}),
		SSEEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}),
		SSEEvent("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}),
	}
	if result.Text != "" {
		events = append(events, SSEEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": messageID, "output_index": 0, "content_index": 0, "delta": result.Text}))
	}
	events = append(events,
		SSEEvent("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": messageID, "output_index": 0, "content_index": 0, "text": result.Text}),
		SSEEvent("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": result.Text, "annotations": []any{}}}),
		SSEEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": messageID, "type": "message", "status": "completed", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": result.Text, "annotations": []any{}}}}}),
		SSEEvent("response.completed", map[string]any{"type": "response.completed", "response": ResponsesResponse(result, model)}),
	)
	return events
}

func ResponsesStreamStart(result ports.ChatResult, model string) [][]byte {
	events := ResponsesSSE(result, model)
	if len(events) > 4 {
		return events[:4]
	}
	return events
}

func ResponsesStreamDelta(result ports.ChatResult, text string) []byte {
	return SSEEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_" + result.ID, "output_index": 0, "content_index": 0, "delta": text})
}

func ResponsesStreamFinish(result ports.ChatResult, model string) [][]byte {
	messageID := "msg_" + result.ID
	return [][]byte{
		SSEEvent("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": messageID, "output_index": 0,
			"content_index": 0, "text": result.Text,
		}),
		SSEEvent("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": messageID, "output_index": 0,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": result.Text, "annotations": []any{}},
		}),
		SSEEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{
				"id": messageID, "type": "message", "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": result.Text, "annotations": []any{}}},
			},
		}),
		SSEEvent("response.completed", map[string]any{
			"type": "response.completed", "response": ResponsesResponse(result, model),
		}),
	}
}

func ResponsesStreamError(message string) []byte {
	return SSEEvent("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"type": "server_error", "message": message}}})
}

func responseInputText(value any, field string) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	items, ok := value.([]any)
	if !ok {
		return "", &RequestError{Message: field + " must be text or an array of input_text"}
	}
	var result strings.Builder
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "input_text" {
			return "", &RequestError{Message: fmt.Sprintf("%s[%d] only supports input_text", field, index)}
		}
		text, ok := item["text"].(string)
		if !ok {
			return "", &RequestError{Message: fmt.Sprintf("%s[%d].text must be a string", field, index)}
		}
		result.WriteString(text)
	}
	return result.String(), nil
}

func optionalPositiveInt(payload map[string]any, field string) (*int, error) {
	value, exists := payload[field]
	if !exists {
		return nil, nil
	}
	result, ok := positiveInt(value)
	if !ok {
		return nil, &RequestError{Message: field + " must be a positive integer"}
	}
	return &result, nil
}
