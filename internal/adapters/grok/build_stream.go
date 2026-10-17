package grok

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/infrastructure/sse"
	"github.com/XuLaiQ/all2api/internal/ports"
)

var errBuildStreamDone = errors.New("build stream done")

// parseBuildResponsesSSE reads the xAI Responses API stream used by Grok
// Build (cli-chat-proxy /v1/responses). Only output_text deltas are visible
// text; reasoning deltas go to Reasoning; tool/function argument deltas are
// ignored. response.completed/incomplete/failed end the stream.
func parseBuildResponsesSSE(reader io.Reader, emit func(ports.StreamChunk) error) error {
	id := ""
	created := time.Now().Unix()
	streamedText := false
	finished := false
	finish := func(reason string, usage *ports.Usage) error {
		if finished {
			return errBuildStreamDone
		}
		finished = true
		if err := emit(ports.StreamChunk{ID: id, CreatedAt: created, FinishReason: reason, Usage: usage}); err != nil {
			return err
		}
		return errBuildStreamDone
	}
	err := sse.Read(reader, func(event sse.Event) error {
		data := strings.TrimSpace(event.Data)
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			return finish("stop", nil)
		}
		var payload map[string]any
		if json.Unmarshal([]byte(data), &payload) != nil {
			return nil
		}
		eventType, _ := payload["type"].(string)
		if eventType == "" {
			eventType = event.Name
		}
		response, _ := payload["response"].(map[string]any)
		if value := stringValue(response["id"]); value != "" {
			id = value
		}
		switch {
		case eventType == "response.output_text.delta":
			if delta, _ := payload["delta"].(string); delta != "" {
				streamedText = true
				return emit(ports.StreamChunk{ID: id, CreatedAt: created, Text: delta})
			}
		case strings.Contains(eventType, "reasoning") && strings.HasSuffix(eventType, ".delta"):
			if delta, _ := payload["delta"].(string); delta != "" {
				return emit(ports.StreamChunk{ID: id, CreatedAt: created, Reasoning: delta})
			}
		case eventType == "response.output_text.done":
			if text, _ := payload["text"].(string); text != "" && !streamedText {
				streamedText = true
				return emit(ports.StreamChunk{ID: id, CreatedAt: created, Text: text})
			}
		case eventType == "response.completed" || eventType == "response.incomplete":
			if !streamedText {
				if text := buildResponseText(response); text != "" {
					streamedText = true
					if err := emit(ports.StreamChunk{ID: id, CreatedAt: created, Text: text}); err != nil {
						return err
					}
				}
			}
			reason := "stop"
			if eventType == "response.incomplete" {
				reason = "length"
			}
			return finish(reason, buildUsage(response["usage"]))
		case eventType == "response.failed" || eventType == "error":
			return buildStreamError(payload, response)
		}
		return nil
	})
	if errors.Is(err, errBuildStreamDone) {
		return nil
	}
	if err != nil {
		return err
	}
	if !finished {
		return emit(ports.StreamChunk{ID: id, CreatedAt: created, FinishReason: "stop"})
	}
	return nil
}

func buildUsage(value any) *ports.Usage {
	usage, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &ports.Usage{PromptTokens: intNumber(usage["input_tokens"]), CompletionTokens: intNumber(usage["output_tokens"])}
}

func buildStreamError(payload, response map[string]any) error {
	errorValue, _ := payload["error"].(map[string]any)
	if errorValue == nil {
		errorValue, _ = response["error"].(map[string]any)
	}
	message := firstString(errorValue, "message")
	if message == "" {
		message = firstString(payload, "message")
	}
	if message == "" {
		message = "Grok Build response failed"
	}
	code := strings.ToLower(firstString(errorValue, "code", "type"))
	lower := strings.ToLower(message)
	status := http.StatusBadGateway
	switch {
	case strings.Contains(code, "rate") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "quota") || strings.Contains(lower, "usage limit"):
		status = http.StatusTooManyRequests
	case strings.Contains(code, "auth") || strings.Contains(lower, "unauthor"):
		status = http.StatusUnauthorized
	case strings.Contains(code, "invalid") || strings.Contains(lower, "model not found"):
		status = http.StatusBadRequest
	}
	return &GatewayError{Status: status, Code: code, Message: message}
}
