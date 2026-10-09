package protocols

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func NormalizeAnthropic(payload map[string]any) (ports.ChatInput, error) {
	allowed := map[string]bool{
		"model": true, "messages": true, "max_tokens": true, "system": true,
		"stream": true, "temperature": true, "top_p": true, "stop_sequences": true,
		"metadata": true, "tools": true, "tool_choice": true,
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
	maxTokens, ok := positiveInt(payload["max_tokens"])
	if !ok {
		return ports.ChatInput{}, &RequestError{Message: "max_tokens must be a positive integer"}
	}
	stream, err := optionalBool(payload, "stream")
	if err != nil {
		return ports.ChatInput{}, err
	}
	messagesValue, ok := payload["messages"].([]any)
	if !ok || len(messagesValue) == 0 {
		return ports.ChatInput{}, &RequestError{Message: "messages must be a non-empty array"}
	}
	result := ports.ChatInput{Model: model, MaxTokens: &maxTokens, Stream: stream}
	if system, ok := payload["system"]; ok {
		text, err := textBlocks(system, "system")
		if err != nil {
			return ports.ChatInput{}, err
		}
		result.Messages = append(result.Messages, ports.ChatMessage{Role: "system", Content: text})
	}
	pending := map[string]bool{}
	for index, raw := range messagesValue {
		message, ok := raw.(map[string]any)
		if !ok || (message["role"] != "user" && message["role"] != "assistant") {
			return ports.ChatInput{}, &RequestError{Message: fmt.Sprintf("messages[%d].role must be user or assistant", index)}
		}
		role := message["role"].(string)
		content, toolCalls, toolResult, err := anthropicContent(role, message["content"], fmt.Sprintf("messages[%d].content", index))
		if err != nil {
			return ports.ChatInput{}, err
		}
		if role == "assistant" {
			for _, call := range toolCalls {
				if pending[call.ID] {
					return ports.ChatInput{}, &RequestError{Message: "tool_use ids must be unique"}
				}
				pending[call.ID] = true
			}
		}
		if role == "user" {
			for _, call := range toolResult {
				if !pending[call.ToolCallID] {
					return ports.ChatInput{}, &RequestError{Message: "tool_result must reference a previous tool_use"}
				}
				delete(pending, call.ToolCallID)
			}
		}
		if content != "" || len(toolCalls) > 0 || len(toolResult) > 0 {
			result.Messages = append(result.Messages, ports.ChatMessage{Role: role, Content: content, ToolCalls: toolCalls})
			result.Messages = append(result.Messages, toolResult...)
		}
	}
	if result.Temperature, err = optionalNumber(payload, "temperature"); err != nil {
		return ports.ChatInput{}, err
	}
	if result.TopP, err = optionalNumber(payload, "top_p"); err != nil {
		return ports.ChatInput{}, err
	}
	if stop, exists := payload["stop_sequences"]; exists {
		values, ok := stop.([]any)
		if !ok {
			return ports.ChatInput{}, &RequestError{Message: "stop_sequences must be an array of strings"}
		}
		for _, value := range values {
			item, ok := value.(string)
			if !ok {
				return ports.ChatInput{}, &RequestError{Message: "stop_sequences must be an array of strings"}
			}
			result.Stop = append(result.Stop, item)
		}
	}
	if tools, exists := payload["tools"]; exists {
		result.Tools, err = normalizeTools(tools)
		if err != nil {
			return ports.ChatInput{}, err
		}
	}
	if choice, exists := payload["tool_choice"]; exists {
		result.ToolChoice, err = normalizeToolChoice(choice, result.Tools)
		if err != nil {
			return ports.ChatInput{}, err
		}
	}
	return result, nil
}

func AnthropicResponse(result ports.ChatResult, model string) map[string]any {
	stop := "end_turn"
	if result.FinishReason == "length" {
		stop = "max_tokens"
	} else if result.FinishReason == "tool_calls" {
		stop = "tool_use"
	}
	inputTokens, outputTokens := 0, 0
	if result.Usage != nil {
		inputTokens, outputTokens = result.Usage.PromptTokens, result.Usage.CompletionTokens
	}
	content := []map[string]any{}
	if result.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": result.Text})
	}
	return map[string]any{
		"id": "msg_" + result.ID, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": inputTokens, "output_tokens": outputTokens},
	}
}

func AnthropicSSE(result ports.ChatResult, model string) [][]byte {
	message := AnthropicResponse(result, model)
	message["content"] = []map[string]any{}
	events := [][]byte{
		SSEEvent("message_start", map[string]any{"type": "message_start", "message": message}),
	}
	if result.Text != "" {
		events = append(events,
			SSEEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
			SSEEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": result.Text}}),
			SSEEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		)
	}
	events = append(events,
		SSEEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": message["stop_reason"], "stop_sequence": nil}, "usage": message["usage"]}),
		SSEEvent("message_stop", map[string]any{"type": "message_stop"}),
	)
	return events
}

func AnthropicStreamStart(result ports.ChatResult, model string) [][]byte {
	message := AnthropicResponse(result, model)
	message["content"] = []map[string]any{}
	return [][]byte{
		SSEEvent("message_start", map[string]any{"type": "message_start", "message": message}),
		SSEEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
	}
}

func AnthropicStreamDelta(text string) []byte {
	return SSEEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
}

func AnthropicStreamFinish(result ports.ChatResult, model string) [][]byte {
	message := AnthropicResponse(result, model)
	return [][]byte{
		SSEEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		SSEEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": message["stop_reason"], "stop_sequence": nil}, "usage": message["usage"]}),
		SSEEvent("message_stop", map[string]any{"type": "message_stop"}),
	}
}

func AnthropicStreamError(message string) [][]byte {
	return [][]byte{
		SSEEvent("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}}),
		SSEEvent("message_stop", map[string]any{"type": "message_stop"}),
	}
}

func anthropicContent(role string, value any, field string) (string, []ports.ToolCall, []ports.ChatMessage, error) {
	if text, ok := value.(string); ok {
		return text, nil, nil, nil
	}
	blocks, ok := value.([]any)
	if !ok {
		return "", nil, nil, &RequestError{Message: field + " must be text or an array of content blocks"}
	}
	var text strings.Builder
	var calls []ports.ToolCall
	var results []ports.ChatMessage
	for index, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return "", nil, nil, &RequestError{Message: fmt.Sprintf("%s[%d] must be an object", field, index)}
		}
		typeName, _ := block["type"].(string)
		switch typeName {
		case "text":
			value, ok := block["text"].(string)
			if !ok {
				return "", nil, nil, &RequestError{Message: field + ".text must be a string"}
			}
			text.WriteString(value)
		case "tool_use":
			if role != "assistant" {
				return "", nil, nil, &RequestError{Message: "tool_use is only supported in assistant messages"}
			}
			id, idOK := block["id"].(string)
			name, nameOK := block["name"].(string)
			input, inputOK := block["input"].(map[string]any)
			if !idOK || id == "" || !nameOK || name == "" || !inputOK {
				return "", nil, nil, &RequestError{Message: field + " tool_use requires id, name and object input"}
			}
			encoded, _ := json.Marshal(input)
			calls = append(calls, ports.ToolCall{ID: id, Name: name, Arguments: string(encoded)})
		case "tool_result":
			if role != "user" {
				return "", nil, nil, &RequestError{Message: "tool_result is only supported in user messages"}
			}
			id, ok := block["tool_use_id"].(string)
			if !ok || id == "" {
				return "", nil, nil, &RequestError{Message: field + " tool_result requires tool_use_id"}
			}
			content, err := textBlocks(block["content"], field+".content")
			if err != nil {
				return "", nil, nil, err
			}
			results = append(results, ports.ChatMessage{Role: "tool", Content: content, ToolCallID: id})
		default:
			return "", nil, nil, &RequestError{Message: field + " has an unsupported type"}
		}
	}
	return text.String(), calls, results, nil
}

func textBlocks(value any, field string) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	blocks, ok := value.([]any)
	if !ok {
		return "", &RequestError{Message: field + " must be text or an array of text blocks"}
	}
	var result strings.Builder
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" {
			return "", &RequestError{Message: field + " only supports text content blocks"}
		}
		text, ok := block["text"].(string)
		if !ok {
			return "", &RequestError{Message: field + " text blocks require a text value"}
		}
		result.WriteString(text)
	}
	return result.String(), nil
}
