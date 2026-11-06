package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gatewayapp "github.com/XuLaiQ/all2api/internal/application/gateway"
	"github.com/XuLaiQ/all2api/internal/infrastructure/tokens"
	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/XuLaiQ/all2api/internal/protocols"
)

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	items, err := s.gateway.Models(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	if err != nil {
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		publicID := item.PublicID
		if publicID == "" {
			publicID = item.UpstreamID
		}
		entry := map[string]any{
			"id":       item.Channel + "/" + publicID,
			"object":   "model",
			"created":  0,
			"owned_by": item.Channel,
		}
		if item.DisplayName != "" {
			entry["display_name"] = item.DisplayName
		}
		data = append(data, entry)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) unsupportedDataCapability(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, r, http.StatusNotImplemented, "capability_not_supported", "this data-plane capability is not implemented in the Go runtime", nil)
}

func (s *Server) capability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	payload, err := decodeProtocolBody(w, r)
	if err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	capability := "image"
	if strings.Contains(r.URL.Path, "/video/") {
		capability = "video"
	} else if strings.HasSuffix(r.URL.Path, "/search") {
		capability = "search"
	}
	model, _ := payload["model"].(string)
	prompt, _ := payload["prompt"].(string)
	input := ports.CapabilityInput{Capability: capability, Model: model, Prompt: prompt}
	input.Ratio, _ = payload["ratio"].(string)
	input.Size, _ = payload["size"].(string)
	input.Quality, _ = payload["quality"].(string)
	if duration, ok := payload["duration"].(float64); ok {
		value := int(duration)
		input.Duration = &value
	}
	if count, ok := payload["n"].(float64); ok {
		input.Count = int(count)
	}
	if images, ok := payload["base64_images"].([]any); ok {
		for _, image := range images {
			if value, ok := image.(string); ok {
				input.Images = append(input.Images, value)
			}
		}
	}
	if input.Model == "" && capability == "search" {
		input.Model = "chatgpt/auto"
	}
	if input.Model == "" || input.Prompt == "" {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "model and prompt are required", nil)
		return
	}
	key, authErr := s.gateway.Authenticate(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	if authErr != nil {
		s.writeOpenAIServiceError(w, r, authErr)
		return
	}
	channel, _, validModel := strings.Cut(input.Model, "/")
	registration, registered := s.registry.Get(channel)
	if !validModel || !registered || !manifestHasCapability(registration.Manifest.Capabilities, capability) {
		writeOpenAIError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	started := time.Now()
	key, result, err := s.gateway.CapabilityWithKey(r.Context(), key, input)
	if err != nil {
		s.recordGatewayFailure(r, key, input.Model, err, false, started)
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	if result.StatusCode >= 400 {
		s.recordCapabilityRequest(r, key, input.Model, result.StatusCode, "capability_error", started)
		writeJSON(w, r, result.StatusCode, result.Body)
		return
	}
	rewritten, _, storeErr := s.storeCapabilityMedia(r.Context(), fileActor(key), channel, input.Model, capability, result.Body, true, nil, nil)
	if storeErr != nil {
		s.recordCapabilityRequest(r, key, input.Model, http.StatusBadGateway, "media_store_error", started)
		writeOpenAIError(w, r, http.StatusBadGateway, "media_store_error", "generated media could not be stored", nil)
		return
	}
	result.Body = rewritten
	s.recordCapabilityRequest(r, key, input.Model, result.StatusCode, "", started)
	writeJSON(w, r, result.StatusCode, result.Body)
}

func manifestHasCapability(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	// ReasoningEffort follows OpenAI; ThinkingEffort is the chatgpt2api alias.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ThinkingEffort  string `json:"thinking_effort,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	var body chatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "request body is invalid", nil)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object", nil)
		return
	}
	if strings.TrimSpace(body.Model) == "" || len(body.Messages) == 0 {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "model and messages are required", nil)
		return
	}
	messages := make([]ports.ChatMessage, 0, len(body.Messages))
	for _, message := range body.Messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "unsupported message role", nil)
			return
		}
		messages = append(messages, ports.ChatMessage{Role: message.Role, Content: message.Content})
	}
	started := time.Now()
	chatInput := ports.ChatInput{
		Model: body.Model, Messages: messages, MaxTokens: body.MaxTokens,
		Temperature: body.Temperature, TopP: body.TopP,
		ReasoningEffort: strings.TrimSpace(body.ThinkingEffort),
	}
	if chatInput.ReasoningEffort == "" {
		chatInput.ReasoningEffort = strings.TrimSpace(body.ReasoningEffort)
	}
	if body.Stream {
		result := ports.ChatResult{ID: "chatcmpl_stream", FinishReason: "stop", CreatedAt: time.Now().Unix()}
		var flusher http.Flusher
		streamStarted := false
		firstChunk := true
		startStream := func() error {
			if streamStarted {
				return nil
			}
			var ok bool
			flusher, ok = beginProtocolStream(w)
			if !ok {
				return fmt.Errorf("streaming is unavailable")
			}
			streamStarted = true
			return nil
		}
		key, streamErr := s.gateway.ChatStream(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), chatInput, func(chunk ports.StreamChunk) error {
			if err := startStream(); err != nil {
				return err
			}
			if result.ID == "chatcmpl_stream" && chunk.ID != "" {
				result.ID = chunk.ID
			}
			if chunk.CreatedAt != 0 {
				result.CreatedAt = chunk.CreatedAt
			}
			if chunk.FinishReason != "" {
				result.FinishReason = chunk.FinishReason
			}
			if chunk.Usage != nil {
				result.Usage = chunk.Usage
			}
			result.UpstreamModel, result.Channel, result.RouteAlias, result.FallbackDepth = chunk.UpstreamModel, chunk.Channel, chunk.RouteAlias, chunk.FallbackDepth
			if chunk.Text == "" && chunk.Reasoning == "" {
				return nil
			}
			result.Text += chunk.Text
			result.Reasoning += chunk.Reasoning
			delta := map[string]any{}
			if chunk.Text != "" {
				delta["content"] = chunk.Text
			}
			if chunk.Reasoning != "" {
				delta["reasoning_content"] = chunk.Reasoning
			}
			if firstChunk {
				delta["role"] = "assistant"
				firstChunk = false
			}
			writeSSE(w, map[string]any{"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": body.Model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}}})
			flusher.Flush()
			return nil
		})
		if streamErr != nil {
			if streamStarted {
				interrupted := &gatewayapp.Error{Status: http.StatusBadGateway, Code: "stream_interrupted", Message: "stream interrupted"}
				s.recordGatewayFailure(r, key, body.Model, interrupted, true, started)
				writeSSE(w, map[string]any{"error": map[string]any{"type": "api_error", "code": "stream_interrupted", "message": "stream interrupted"}})
				flusher.Flush()
			} else {
				s.recordGatewayFailure(r, key, body.Model, streamErr, true, started)
				s.writeOpenAIServiceError(w, r, streamErr)
			}
			return
		}
		if err := startStream(); err != nil {
			writeOpenAIError(w, r, http.StatusInternalServerError, "request_error", err.Error(), nil)
			return
		}
		writeSSE(w, map[string]any{"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": body.Model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": result.FinishReason}}})
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		s.recordGatewaySuccess(r, key, body.Model, chatInput.Messages, result, true, started)
		return
	}
	key, result, err := s.gateway.Chat(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), chatInput)
	if err != nil {
		s.recordGatewayFailure(r, key, body.Model, err, body.Stream, started)
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	response := map[string]any{
		"id":      result.ID,
		"object":  "chat.completion",
		"created": result.CreatedAt,
		"model":   body.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       assistantMessage(result),
			"finish_reason": result.FinishReason,
		}},
	}
	if result.Usage != nil {
		response["usage"] = map[string]int{
			"prompt_tokens":     result.Usage.PromptTokens,
			"completion_tokens": result.Usage.CompletionTokens,
			"total_tokens":      result.Usage.PromptTokens + result.Usage.CompletionTokens,
		}
	}
	writeJSON(w, r, http.StatusOK, response)
	s.recordGatewaySuccess(r, key, body.Model, chatInput.Messages, result, false, started)
}

// assistantMessage renders a non-streaming OpenAI assistant message; upstream
// thinking is exposed as reasoning_content (DeepSeek/OpenRouter convention).
func assistantMessage(result ports.ChatResult) map[string]any {
	message := map[string]any{"role": "assistant", "content": result.Text}
	if result.Reasoning != "" {
		message["reasoning_content"] = result.Reasoning
	}
	return message
}

func (s *Server) writeChatStreamChunks(w http.ResponseWriter, model string, chunks []ports.StreamChunk, result ports.ChatResult) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	for _, chunk := range chunks {
		id := chunk.ID
		if id == "" {
			id = result.ID
		}
		created := chunk.CreatedAt
		if created == 0 {
			created = result.CreatedAt
		}
		writeSSE(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": chunk.Text}, "finish_reason": nil}}})
		flusher.Flush()
	}
	writeSSE(w, map[string]any{"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": result.FinishReason}}})
	flusher.Flush()
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed")
		return
	}
	payload, err := decodeProtocolBody(w, r)
	if err != nil {
		writeAnthropicError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input, err := protocols.NormalizeAnthropic(payload)
	if err != nil {
		writeAnthropicError(w, r, http.StatusUnprocessableEntity, "invalid_request", err.Error())
		return
	}
	started := time.Now()
	if input.Stream {
		result := ports.ChatResult{ID: "chatcmpl_stream", FinishReason: "stop", CreatedAt: time.Now().Unix()}
		var flusher http.Flusher
		streamStarted := false
		startStream := func() error {
			if streamStarted {
				return nil
			}
			var ok bool
			flusher, ok = beginProtocolStream(w)
			if !ok {
				return fmt.Errorf("streaming is unavailable")
			}
			writeProtocolEventsNow(w, flusher, protocols.AnthropicStreamStart(result, input.Model))
			streamStarted = true
			return nil
		}
		var textBuilder strings.Builder
		key, streamErr := s.gateway.ChatStream(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input, func(chunk ports.StreamChunk) error {
			if err := startStream(); err != nil {
				return err
			}
			if result.ID == "chatcmpl_stream" && chunk.ID != "" {
				result.ID = chunk.ID
			}
			if chunk.CreatedAt != 0 {
				result.CreatedAt = chunk.CreatedAt
			}
			if chunk.FinishReason != "" {
				result.FinishReason = chunk.FinishReason
			}
			if chunk.Usage != nil {
				result.Usage = chunk.Usage
			}
			result.Channel, result.UpstreamModel, result.RouteAlias, result.FallbackDepth = chunk.Channel, chunk.UpstreamModel, chunk.RouteAlias, chunk.FallbackDepth
			if chunk.Text == "" {
				return nil
			}
			textBuilder.WriteString(chunk.Text)
			writeProtocolEventsNow(w, flusher, [][]byte{protocols.AnthropicStreamDelta(chunk.Text)})
			return nil
		})
		result.Text = textBuilder.String()
		if streamErr != nil {
			s.recordGatewayFailure(r, key, input.Model, streamErr, true, started)
			if streamStarted {
				writeProtocolEventsNow(w, flusher, protocols.AnthropicStreamError(streamErr.Error()))
			} else {
				s.writeAnthropicServiceError(w, r, streamErr)
			}
			return
		}
		if err := startStream(); err != nil {
			writeAnthropicError(w, r, http.StatusInternalServerError, "request_error", err.Error())
			return
		}
		writeProtocolEventsNow(w, flusher, protocols.AnthropicStreamFinish(result, input.Model))
		s.recordGatewaySuccess(r, key, input.Model, input.Messages, result, true, started)
		return
	}
	key, result, err := s.gateway.Chat(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input)
	if err != nil {
		s.recordGatewayFailure(r, key, input.Model, err, input.Stream, started)
		s.writeAnthropicServiceError(w, r, err)
		return
	}
	w.Header().Set("request-id", requestID(r.Context()))
	writeJSON(w, r, http.StatusOK, protocols.AnthropicResponse(result, input.Model))
	s.recordGatewaySuccess(r, key, input.Model, input.Messages, result, false, started)
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	payload, err := decodeProtocolBody(w, r)
	if err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	input, err := protocols.NormalizeResponses(payload)
	if err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	started := time.Now()
	if input.Stream {
		result := ports.ChatResult{ID: "chatcmpl_stream", FinishReason: "stop", CreatedAt: time.Now().Unix()}
		var flusher http.Flusher
		streamStarted := false
		startStream := func() error {
			if streamStarted {
				return nil
			}
			var ok bool
			flusher, ok = beginProtocolStream(w)
			if !ok {
				return fmt.Errorf("streaming is unavailable")
			}
			writeProtocolEventsNow(w, flusher, protocols.ResponsesStreamStart(result, input.Model))
			streamStarted = true
			return nil
		}
		var textBuilder strings.Builder
		key, streamErr := s.gateway.ChatStream(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input, func(chunk ports.StreamChunk) error {
			if err := startStream(); err != nil {
				return err
			}
			if result.ID == "chatcmpl_stream" && chunk.ID != "" {
				result.ID = chunk.ID
			}
			if chunk.CreatedAt != 0 {
				result.CreatedAt = chunk.CreatedAt
			}
			if chunk.FinishReason != "" {
				result.FinishReason = chunk.FinishReason
			}
			if chunk.Usage != nil {
				result.Usage = chunk.Usage
			}
			result.Channel, result.UpstreamModel, result.RouteAlias, result.FallbackDepth = chunk.Channel, chunk.UpstreamModel, chunk.RouteAlias, chunk.FallbackDepth
			if chunk.Text == "" {
				return nil
			}
			textBuilder.WriteString(chunk.Text)
			writeProtocolEventsNow(w, flusher, [][]byte{protocols.ResponsesStreamDelta(result, chunk.Text)})
			return nil
		})
		result.Text = textBuilder.String()
		if streamErr != nil {
			s.recordGatewayFailure(r, key, input.Model, streamErr, true, started)
			if streamStarted {
				writeProtocolEventsNow(w, flusher, [][]byte{protocols.ResponsesStreamError(streamErr.Error())})
			} else {
				s.writeOpenAIServiceError(w, r, streamErr)
			}
			return
		}
		if err := startStream(); err != nil {
			writeOpenAIError(w, r, http.StatusInternalServerError, "request_error", err.Error(), nil)
			return
		}
		writeProtocolEventsNow(w, flusher, protocols.ResponsesStreamFinish(result, input.Model))
		s.recordGatewaySuccess(r, key, input.Model, input.Messages, result, true, started)
		return
	}
	key, result, err := s.gateway.Chat(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input)
	if err != nil {
		s.recordGatewayFailure(r, key, input.Model, err, input.Stream, started)
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, protocols.ResponsesResponse(result, input.Model))
	s.recordGatewaySuccess(r, key, input.Model, input.Messages, result, false, started)
}

func (s *Server) recordGatewaySuccess(r *http.Request, key ports.APIKey, model string, messages []ports.ChatMessage, result ports.ChatResult, stream bool, started time.Time) {
	if key.ID == 0 {
		return
	}
	channel := result.Channel
	if channel == "" {
		channel, _ = splitGatewayModel(model)
	}
	record := ports.RequestRecord{
		RequestID: requestID(r.Context()), KeyID: key.ID,
		Channel: channel, Model: model, UpstreamModel: result.UpstreamModel, RouteAlias: result.RouteAlias, FallbackDepth: result.FallbackDepth, Status: http.StatusOK,
		Stream: stream, StartedAt: started,
		LatencyMS: time.Since(started).Milliseconds(),
	}
	applyUsage(&record, messages, result, model)
	s.recordRequest(r.Context(), record)
}

// applyUsage stores upstream-reported usage when present; otherwise it records
// a local tiktoken estimate so dashboards are not silently zero.
func applyUsage(record *ports.RequestRecord, messages []ports.ChatMessage, result ports.ChatResult, model string) {
	if result.Usage != nil {
		record.PromptTokens = result.Usage.PromptTokens
		record.CompletionTokens = result.Usage.CompletionTokens
		record.UsageReported = true
		record.UsageKind = "reported"
		return
	}
	estimateModel := result.UpstreamModel
	if estimateModel == "" {
		estimateModel = model
	}
	record.PromptTokens, record.CompletionTokens = tokens.Estimate(messages, result.Text, estimateModel)
	record.UsageKind = "estimated"
}

func (s *Server) recordGatewayFailure(r *http.Request, key ports.APIKey, model string, err error, stream bool, started time.Time) {
	if key.ID == 0 {
		return
	}
	status, code := http.StatusInternalServerError, "api_error"
	var serviceError *gatewayapp.Error
	if errors.As(err, &serviceError) {
		status, code = serviceError.Status, serviceError.Code
	}
	channel, upstream := splitGatewayModel(model)
	routeAlias := ""
	fallbackDepth := 0
	if serviceError != nil {
		if serviceError.Channel != "" {
			channel = serviceError.Channel
		}
		if serviceError.UpstreamModel != "" {
			upstream = serviceError.UpstreamModel
		}
		routeAlias = serviceError.RouteAlias
		fallbackDepth = serviceError.FallbackDepth
	}
	if routeAlias == "" && channel == "" {
		routeAlias = model
	}
	s.recordRequest(r.Context(), ports.RequestRecord{
		RequestID: requestID(r.Context()), KeyID: key.ID,
		Channel: channel, Model: model, UpstreamModel: upstream, RouteAlias: routeAlias, FallbackDepth: fallbackDepth, Status: status,
		ErrorKind: code, Error: code, Stream: stream, UsageKind: "unknown",
		StartedAt: started, LatencyMS: time.Since(started).Milliseconds(),
	})
}

func (s *Server) recordCapabilityRequest(r *http.Request, key ports.APIKey, model string, status int, errorCode string, started time.Time) {
	if key.ID == 0 {
		return
	}
	channel, upstream := splitGatewayModel(model)
	routeAlias := ""
	if channel == "" {
		routeAlias = model
	}
	s.recordRequest(r.Context(), ports.RequestRecord{
		RequestID: requestID(r.Context()), KeyID: key.ID,
		Channel: channel, Model: model, UpstreamModel: upstream, RouteAlias: routeAlias, Status: status,
		ErrorKind: errorCode, Error: errorCode, UsageKind: "unknown",
		StartedAt: started, LatencyMS: time.Since(started).Milliseconds(),
	})
}

func (s *Server) recordRequest(ctx context.Context, record ports.RequestRecord) {
	if err := s.gateway.Record(ctx, record); err != nil && s.logger != nil {
		s.logger.Printf("request record failed request_id=%s error=%v", record.RequestID, err)
	}
}

func splitGatewayModel(model string) (string, string) {
	channel, upstream, ok := strings.Cut(model, "/")
	if !ok {
		return "", model
	}
	return channel, upstream
}

func decodeProtocolBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("request body must contain one JSON object")
	}
	return payload, nil
}

func (s *Server) writeAnthropicServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var serviceError *gatewayapp.Error
	if errors.As(err, &serviceError) {
		w.Header().Set("request-id", requestID(r.Context()))
		writeJSON(w, r, serviceError.Status, protocols.AnthropicError(serviceError.Message, serviceError.Status, serviceError.Code))
		return
	}
	writeAnthropicError(w, r, http.StatusInternalServerError, "api_error", "internal server error")
}

func writeAnthropicError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("request-id", requestID(r.Context()))
	writeJSON(w, r, status, protocols.AnthropicError(message, status, code))
}

func beginProtocolStream(w http.ResponseWriter) (http.Flusher, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	return flusher, true
}

func writeProtocolEventsNow(w http.ResponseWriter, flusher http.Flusher, events [][]byte) {
	for _, event := range events {
		_, _ = w.Write(event)
		flusher.Flush()
	}
}

func (s *Server) writeProtocolEvents(w http.ResponseWriter, events [][]byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	for _, event := range events {
		_, _ = w.Write(event)
		flusher.Flush()
	}
}

func (s *Server) writeChatStream(w http.ResponseWriter, r *http.Request, model string, result ports.ChatResult) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, r, http.StatusInternalServerError, "request_error", "streaming is unavailable", nil)
		return
	}
	writeSSE(w, map[string]any{
		"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": result.Text}, "finish_reason": nil}},
	})
	flusher.Flush()
	writeSSE(w, map[string]any{
		"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": result.FinishReason}},
	})
	flusher.Flush()
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeSSE(w http.ResponseWriter, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
}

func (s *Server) writeOpenAIServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var serviceError *gatewayapp.Error
	if errors.As(err, &serviceError) {
		writeOpenAIError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, serviceError.Headers)
		return
	}
	writeOpenAIError(w, r, http.StatusInternalServerError, "api_error", "internal server error", nil)
}

func writeOpenAIError(w http.ResponseWriter, r *http.Request, status int, code, message string, headers map[string]string) {
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	errorType := "api_error"
	if status == http.StatusUnauthorized {
		errorType = "authentication_error"
	} else if status == http.StatusBadRequest {
		errorType = "invalid_request_error"
	} else if status == http.StatusForbidden {
		errorType = "permission_error"
	}
	writeJSON(w, r, status, map[string]any{
		"error": map[string]any{
			"message": message, "type": errorType, "param": nil, "code": code,
		},
	})
}
