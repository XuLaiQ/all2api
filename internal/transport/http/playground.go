package httptransport

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	gatewayapp "github.com/XuLaiQ/all2api/internal/application/gateway"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) playgroundConversations(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	page, size, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	rows, total, err := s.playground.ListConversations(r.Context(), user.Username, page, size)
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": rows, "pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size}})
}

func (s *Server) playgroundConversation(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	id := r.PathValue("conversation_id")
	switch r.Method {
	case http.MethodGet:
		item, err := s.playground.GetConversation(r.Context(), user.Username, id)
		if err != nil {
			writeAdminError(w, r, 404, "not_found", "conversation not found", nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": item})
	case http.MethodDelete:
		if user.Role != "admin" {
			writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
			return
		}
		deleted, err := s.playground.DeleteConversation(r.Context(), user.Username, id, ports.AuditEvent{Actor: user.Username, Action: "delete_playground_conversation", IP: s.clientIP(r)})
		if err != nil {
			writeAdminError(w, r, 404, "not_found", "conversation not found", nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": map[string]any{"id": id, "deleted": true, "request_records_deleted": deleted}})
	default:
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
	}
}

func (s *Server) playgroundRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	page, size, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	rows, total, err := s.playground.ListRuns(r.Context(), page, size)
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{
		"data":       rows,
		"pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size},
	})
}

func (s *Server) unsupportedAdminCapability(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r, r.Method != http.MethodGet); !ok {
		return
	}
	writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this management capability is not implemented in the Go runtime", nil)
}

func (s *Server) playgroundSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel        string `json:"channel"`
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		ConversationID string `json:"conversation_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || body.Prompt == "" {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "channel, model and prompt are required", nil)
		return
	}
	registration, registered := s.registry.Get(body.Channel)
	if !registered || !manifestHasCapability(registration.Manifest.Capabilities, "search") {
		writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	conversationID := body.ConversationID
	if conversationID == "" {
		conversationID = fmt.Sprintf("conv_%d", time.Now().UnixNano())
		if err := s.playground.CreateConversation(r.Context(), ports.PlaygroundConversationSummary{ID: conversationID, Actor: user.Username, Title: body.Prompt, Channel: body.Channel, Model: body.Model, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}); err != nil {
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground conversation could not be created", nil)
			return
		}
	}
	runID := fmt.Sprintf("pg_%d", time.Now().UnixNano())
	conversationRef := conversationID
	if err := s.playground.StartRun(r.Context(), ports.PlaygroundRun{ID: runID, ConversationID: &conversationRef, Actor: user.Username, Channel: body.Channel, Model: body.Model, Status: "running", MessageCount: 1, RequestBytes: len(body.Prompt), CreatedAt: time.Now().Unix()}); err != nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground run could not be created", nil)
		return
	}
	result, err := s.gateway.AdminCapability(r.Context(), ports.CapabilityInput{Capability: "search", Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Prompt: body.Prompt})
	if err != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "capability_error")
		if serviceError, ok := err.(*gatewayapp.Error); ok {
			writeAdminError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, nil)
		} else {
			writeAdminError(w, r, http.StatusBadGateway, "capability_error", "playground search failed", nil)
		}
		return
	}
	status := result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	encoded, _ := json.Marshal(result.Body)
	_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: "搜索结果已返回。", Model: body.Model, RawResponse: json.RawMessage(encoded), CreatedAt: time.Now().UnixNano()})
	_ = s.playground.FinishRun(r.Context(), runID, "success", status, "")
	writeJSON(w, r, status, map[string]any{"data": result.Body})
}

func (s *Server) playgroundEditableFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
		return
	}
	if s.playground == nil || s.media == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel string   `json:"channel"`
		Model   string   `json:"model"`
		Kind    string   `json:"kind"`
		Prompt  string   `json:"prompt"`
		Images  []string `json:"base64_images"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 140<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || body.Prompt == "" || (body.Kind != "ppt" && body.Kind != "psd") {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "channel, model, kind and prompt are required", nil)
		return
	}
	registration, registered := s.registry.Get(body.Channel)
	if !registered || !manifestHasCapability(registration.Manifest.Capabilities, "editable") {
		writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	result, err := s.gateway.AdminCapability(r.Context(), ports.CapabilityInput{Capability: "editable", Kind: body.Kind, Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Prompt: body.Prompt, Images: body.Images})
	if err != nil {
		if serviceError, ok := err.(*gatewayapp.Error); ok {
			writeAdminError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, nil)
		} else {
			writeAdminError(w, r, http.StatusBadGateway, "capability_error", "editable file generation failed", nil)
		}
		return
	}
	bodyMap, ok := result.Body.(map[string]any)
	if !ok {
		writeAdminError(w, r, http.StatusBadGateway, "capability_error", "editable response is invalid", nil)
		return
	}
	rawFiles, _ := bodyMap["files"].([]map[string]any)
	assets := make([]map[string]any, 0, len(rawFiles))
	primaryURL, zipURL := "", ""
	for _, raw := range rawFiles {
		name := stringValueAny(raw["name"])
		content := stringValueAny(raw["content_base64"])
		if name == "" || content == "" {
			continue
		}
		data, decodeErr := base64.StdEncoding.DecodeString(content)
		if decodeErr != nil {
			continue
		}
		mime := stringValueAny(raw["mime_type"])
		if mime == "" {
			mime = "application/octet-stream"
		}
		asset, addErr := s.media.AddBytes(r.Context(), ports.MediaAsset{Actor: user.Username, Channel: body.Channel, Model: body.Model, Kind: body.Kind, MIMEType: mime, Filename: name, Metadata: map[string]any{"prompt": body.Prompt, "task_kind": body.Kind}}, data)
		if addErr != nil {
			continue
		}
		payload := mediaAssetPayload(asset, s.media)
		assets = append(assets, payload)
		contentURL, _ := payload["content_url"].(string)
		if strings.HasSuffix(strings.ToLower(name), ".zip") {
			zipURL = contentURL
		} else if primaryURL == "" {
			primaryURL = contentURL
		}
	}
	if len(assets) == 0 {
		writeAdminError(w, r, http.StatusBadGateway, "capability_error", "editable response contained no downloadable files", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": map[string]any{"task_id": "local_" + fmt.Sprintf("%d", time.Now().UnixNano()), "kind": body.Kind, "status": "success", "primary_url": primaryURL, "zip_url": zipURL, "assets": assets}})
}

func stringValueAny(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func (s *Server) playgroundGeneration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel        string `json:"channel"`
		Model          string `json:"model"`
		Capability     string `json:"capability"`
		Prompt         string `json:"prompt"`
		Ratio          string `json:"ratio"`
		Size           string `json:"size"`
		Quality        string `json:"quality"`
		Duration       *int   `json:"duration"`
		Count          int    `json:"n"`
		ConversationID string `json:"conversation_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || body.Prompt == "" || (body.Capability != "image" && body.Capability != "video") {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "channel, model, capability and prompt are required", nil)
		return
	}
	registration, registered := s.registry.Get(body.Channel)
	if !registered || !manifestHasCapability(registration.Manifest.Capabilities, body.Capability) {
		writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	conversationID := body.ConversationID
	if conversationID == "" {
		conversationID = fmt.Sprintf("conv_%d", time.Now().UnixNano())
		if err := s.playground.CreateConversation(r.Context(), ports.PlaygroundConversationSummary{ID: conversationID, Actor: user.Username, Title: body.Prompt, Channel: body.Channel, Model: body.Model, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}); err != nil {
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground conversation could not be created", nil)
			return
		}
	}
	runID := fmt.Sprintf("pg_%d", time.Now().UnixNano())
	conversationRef := conversationID
	if err := s.playground.StartRun(r.Context(), ports.PlaygroundRun{ID: runID, ConversationID: &conversationRef, Actor: user.Username, Channel: body.Channel, Model: body.Model, Status: "running", MessageCount: 1, RequestBytes: len(body.Prompt), CreatedAt: time.Now().Unix()}); err != nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground run could not be created", nil)
		return
	}
	result, err := s.gateway.AdminCapability(r.Context(), ports.CapabilityInput{Capability: body.Capability, Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Prompt: body.Prompt, Ratio: body.Ratio, Size: body.Size, Quality: body.Quality, Duration: body.Duration, Count: body.Count})
	if err != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "capability_error")
		if serviceError, ok := err.(*gatewayapp.Error); ok {
			writeAdminError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, nil)
		} else {
			writeAdminError(w, r, http.StatusBadGateway, "capability_error", "playground generation failed", nil)
		}
		return
	}
	status := result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	runRef := runID
	conversationRef = conversationID
	rewritten, assets, storeErr := s.storeCapabilityMedia(r.Context(), user.Username, body.Channel, body.Model, body.Capability, result.Body, false, &runRef, &conversationRef)
	if storeErr != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", http.StatusBadGateway, "media_store_error")
		writeAdminError(w, r, http.StatusBadGateway, "media_store_error", "generated media could not be stored", nil)
		return
	}
	result.Body = rewritten
	encoded, _ := json.Marshal(result.Body)
	_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: fmt.Sprintf("已生成 %s 结果。", body.Capability), Model: body.Model, RawResponse: json.RawMessage(encoded), CreatedAt: time.Now().UnixNano()})
	if status >= 400 {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", status, "capability_error")
	} else {
		_ = s.playground.FinishRun(r.Context(), runID, "success", status, "")
	}
	assetPayloads := make([]map[string]any, 0, len(assets))
	for _, asset := range assets {
		assetPayloads = append(assetPayloads, mediaAssetPayload(asset, s.media))
	}
	writeJSON(w, r, status, map[string]any{"data": map[string]any{"run_id": runID, "conversation_id": conversationID, "channel": body.Channel, "model": body.Model, "status": map[bool]string{true: "success", false: "failed"}[status < 400], "response_status": status, "response": result.Body, "assets": assetPayloads}})
}

func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel        string        `json:"channel"`
		Model          string        `json:"model"`
		ConversationID string        `json:"conversation_id"`
		Stream         bool          `json:"stream"`
		Messages       []chatMessage `json:"messages"`
		Temperature    *float64      `json:"temperature"`
		MaxTokens      *int          `json:"max_tokens"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || len(body.Messages) == 0 {
		writeAdminError(w, r, 422, "validation_error", "channel, model and messages are required", nil)
		return
	}
	conversationID := body.ConversationID
	if conversationID == "" {
		conversationID = fmt.Sprintf("conv_%d", time.Now().UnixNano())
		title := body.Messages[0].Content
		if len(title) > 80 {
			title = title[:80]
		}
		if title == "" {
			title = "新对话"
		}
		if err := s.playground.CreateConversation(r.Context(), ports.PlaygroundConversationSummary{ID: conversationID, Actor: user.Username, Title: title, Channel: body.Channel, Model: body.Model, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}); err != nil {
			writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
			return
		}
	}
	canonical := make([]ports.ChatMessage, 0, len(body.Messages))
	for _, message := range body.Messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			writeAdminError(w, r, 422, "validation_error", "unsupported message role", nil)
			return
		}
		canonical = append(canonical, ports.ChatMessage{Role: message.Role, Content: message.Content})
	}
	last := body.Messages[len(body.Messages)-1]
	if last.Role == "user" {
		_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "user", Content: last.Content, Model: body.Model, CreatedAt: time.Now().UnixNano()})
	}
	runID := fmt.Sprintf("pg_%d", time.Now().UnixNano())
	requestBytes := 0
	encoded, _ := json.Marshal(body)
	requestBytes = len(encoded)
	conversationRef := conversationID
	if err := s.playground.StartRun(r.Context(), ports.PlaygroundRun{ID: runID, ConversationID: &conversationRef, Actor: user.Username, Channel: body.Channel, Model: body.Model, Status: "running", MessageCount: len(body.Messages), RequestBytes: requestBytes, CreatedAt: time.Now().Unix()}); err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	input := ports.ChatInput{Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Messages: canonical, Temperature: body.Temperature, MaxTokens: body.MaxTokens}
	started := time.Now()
	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, flushOK := w.(http.Flusher)
		if !flushOK {
			_ = s.playground.FinishRun(r.Context(), runID, "failed", 500, "stream_unavailable")
			return
		}
		var answer strings.Builder
		err := s.gateway.AdminChatStream(r.Context(), input, func(chunk ports.StreamChunk) error {
			if chunk.Text == "" {
				return nil // reasoning-only chunks are not shown in the console
			}
			answer.WriteString(chunk.Text)
			data, _ := json.Marshal(map[string]any{"type": "delta", "content": chunk.Text})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			return nil
		})
		if err != nil {
			_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "adapter_error")
			s.recordPlaygroundUsage(r, runID, input, ports.ChatResult{}, 502, true, started)
			data, _ := json.Marshal(map[string]any{"type": "error", "message": err.Error(), "response_status": 502, "error_code": "adapter_error"})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			return
		}
		assistant := answer.String()
		_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: assistant, Model: body.Model, CreatedAt: time.Now().UnixNano()})
		_ = s.playground.FinishRun(r.Context(), runID, "success", 200, "")
		s.recordPlaygroundUsage(r, runID, input, ports.ChatResult{Text: assistant}, 200, true, started)
		done := map[string]any{"type": "done", "run_id": runID, "conversation_id": conversationID, "channel": body.Channel, "model": body.Model, "status": "success", "response_status": 200, "response": map[string]any{"content": assistant}}
		data, _ := json.Marshal(done)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		flusher.Flush()
		return
	}
	result, err := s.gateway.AdminChat(r.Context(), input)
	if err != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "adapter_error")
		s.recordPlaygroundUsage(r, runID, input, ports.ChatResult{}, 502, false, started)
		writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
		return
	}
	s.recordPlaygroundUsage(r, runID, input, result, 200, false, started)
	response := map[string]any{"content": result.Text, "id": result.ID, "usage": result.Usage}
	_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: result.Text, Model: body.Model, RawResponse: response, CreatedAt: time.Now().UnixNano()})
	_ = s.playground.FinishRun(r.Context(), runID, "success", 200, "")
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"run_id": runID, "conversation_id": conversationID, "channel": body.Channel, "model": body.Model, "status": "success", "response_status": 200, "response": response}})
}

// recordPlaygroundUsage writes debug-console traffic into the same request log
// and daily usage tables as gateway traffic (key_id 0 marks admin usage).
func (s *Server) recordPlaygroundUsage(r *http.Request, runID string, input ports.ChatInput, result ports.ChatResult, status int, stream bool, started time.Time) {
	channel, upstream := splitGatewayModel(input.Model)
	record := ports.RequestRecord{
		RequestID: runID, Channel: channel, Model: input.Model, UpstreamModel: upstream,
		Status: status, Stream: stream, StartedAt: started, UsageKind: "unknown",
		LatencyMS: time.Since(started).Milliseconds(),
	}
	if status >= 400 {
		record.ErrorKind, record.Error = "adapter_error", "adapter_error"
	} else {
		if result.UpstreamModel == "" {
			result.UpstreamModel = upstream
		}
		applyUsage(&record, input.Messages, result, input.Model)
	}
	s.recordRequest(r.Context(), record)
}
