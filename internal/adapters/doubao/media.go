package doubao

// Image and video generation through /samantha/chat/completion, ported from
// doubao2api (src/core/client.py generate_image / generate_video and
// src/utils/media_urls.py). Only CMPL events (event_type 2001) carry results;
// event_type 2005 is an upstream error. Video generation is asynchronous: the
// first call returns fin_reason.async_task.id, which is then awaited on
// /samantha/chat/async/stream.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/infrastructure/sse"
	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/google/uuid"
)

const (
	samanthaCompletionPath  = "/samantha/chat/completion"
	samanthaAsyncStreamPath = "/samantha/chat/async/stream"

	eventCompletion = 2001
	eventError      = 2005

	contentTypeImageInput  = 2009
	contentTypeImageResult = 2010
	contentTypeVideoInput  = 2020
	contentTypeVideoResult = 2021
	contentTypeText        = 2001

	skillImage = 3
	skillVideo = 17
)

var (
	imageRequestTimeout = 120 * time.Second
	videoSubmitTimeout  = 90 * time.Second
	videoPollTimeout    = 300 * time.Second
)

// Field precedence from media_urls.py: raw fields are watermark-free; the
// others are CDN templates with the "豆包AI生成" mark baked in.
var (
	rawImageFields         = []string{"image_ori_raw", "image_raw_ori", "image_original_raw"}
	watermarkedImageFields = []string{"image_ori", "image_raw", "image_preview", "preview_img", "image_thumb_ori", "image_thumb_raw", "image_thumb"}
	rawVideoFields         = []string{"video_ori_raw", "video_raw", "video_url_raw"}
	watermarkedVideoFields = []string{"video_url", "play_url", "url"}
)

func (c *Client) CapabilityWithAccount(ctx context.Context, input ports.CapabilityInput, accountID string) (ports.CapabilityResult, error) {
	if input.Capability != "image" && input.Capability != "video" {
		return ports.CapabilityResult{}, fmt.Errorf("Doubao capability is not supported")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return ports.CapabilityResult{}, fmt.Errorf("prompt is required")
	}
	if c.ResolveCredential == nil {
		return ports.CapabilityResult{}, fmt.Errorf("Doubao credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return ports.CapabilityResult{}, err
	}
	if credentialLookup(credentials, "Cookie", "cookie") == "" {
		return ports.CapabilityResult{}, fmt.Errorf("Doubao credential is unavailable")
	}
	var items []map[string]any
	if input.Capability == "image" {
		items, err = c.generateImage(ctx, credentials, input)
	} else {
		items, err = c.generateVideo(ctx, credentials, input)
	}
	if err != nil {
		return ports.CapabilityResult{}, err
	}
	if len(items) == 0 {
		return ports.CapabilityResult{}, fmt.Errorf("Doubao returned no %s result", input.Capability)
	}
	return ports.CapabilityResult{StatusCode: http.StatusOK, Body: map[string]any{"created": time.Now().Unix(), "data": items}}, nil
}

// samanthaPayload mirrors the sent_event built by generate_image/generate_video.
func samanthaPayload(input ports.CapabilityInput, skillID, contentType int) map[string]any {
	content := map[string]any{"text": input.Prompt}
	if input.Ratio != "" {
		content["ratio"] = input.Ratio
	}
	skill := fmt.Sprintf("%d", skillID)
	return map[string]any{
		"messages": []any{map[string]any{
			"content":      mustJSON(content),
			"content_type": contentType,
			"attachments":  []any{},
			"references":   []any{},
			"skill":        map[string]any{"skill_type": skillID, "skill_type_no_default": skillID, "skill_id": skill, "skill_id_no_default": skill},
		}},
		"completion_option": map[string]any{
			"is_regen": false, "with_suggest": true, "need_create_conversation": true, "launch_stage": 1,
			"is_replace": false, "is_delete": false, "is_ai_playground": false, "memory_type": 2,
			"message_from": 0, "use_deep_think": false, "use_auto_cot": false, "resend_for_regen": false,
			"enable_commerce_credit": false, "action_bar_skill_id": skillID,
		},
		"evaluate_option":       map[string]any{"web_ab_params": ""},
		"local_conversation_id": uuid.NewString(),
		"local_message_id":      uuid.NewString(),
	}
}

// samanthaRequest POSTs to a samantha endpoint and buffers the SSE body.
// dropFP removes the fp query parameter, which async/stream does not use.
func (c *Client) samanthaRequest(ctx context.Context, credentials map[string]string, path string, payload any, timeout time.Duration, dropFP bool, operation string) ([]samanthaEvent, error) {
	encoded, _ := json.Marshal(payload)
	request, err := c.newRequest(ctx, http.MethodPost, path, encoded, credentials)
	if err != nil {
		return nil, err
	}
	if dropFP {
		query := request.URL.Query()
		query.Del("fp")
		request.URL.RawQuery = query.Encode()
	}
	request.Header.Set("Accept", "text/event-stream")
	client := *c.HTTPClient
	client.Timeout = timeout
	response, err := client.Do(request)
	if err != nil {
		return nil, ports.NewTransportError("POST "+path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, ports.NewTransportError("POST "+path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPError{Status: response.StatusCode, Operation: operation}
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "event-stream") {
		var object map[string]any
		if json.Unmarshal(bytes.TrimSpace(raw), &object) == nil {
			if code := intAny(object["code"]); code != 0 {
				return nil, &HTTPError{Status: samanthaStatus(fmt.Sprintf("%d", code)), Operation: operation, Code: fmt.Sprintf("%d", code)}
			}
		}
	}
	events := samanthaEvents(raw)
	if err := samanthaEventError(events, operation); err != nil {
		return nil, err
	}
	return events, nil
}

type samanthaEvent struct {
	Name string
	Data map[string]any
}

func samanthaEvents(raw []byte) []samanthaEvent {
	result := make([]samanthaEvent, 0)
	_ = sse.Read(bytes.NewReader(raw), func(event sse.Event) error {
		var value map[string]any
		if json.Unmarshal([]byte(event.Data), &value) == nil {
			result = append(result, samanthaEvent{Name: event.Name, Data: value})
		}
		return nil
	})
	return result
}

// samanthaEventError maps gateway-error frames, event_type 2005 and coded
// payloads to typed HTTP errors.
func samanthaEventError(events []samanthaEvent, operation string) error {
	for _, event := range events {
		if event.Name == "gateway-error" {
			return &HTTPError{Status: http.StatusUnauthorized, Operation: operation, Code: firstNonEmpty(stringAny(event.Data["code"]), "gateway-error")}
		}
		nested, _ := decodeAnyJSON(event.Data["event_data"]).(map[string]any)
		code := stringAny(nested["code"], nested["error_code"], event.Data["code"], event.Data["error_code"])
		if code == "0" {
			code = ""
		}
		if intAny(event.Data["event_type"]) == eventError {
			return &HTTPError{Status: samanthaStatus(code), Operation: operation, Code: firstNonEmpty(code, "2005")}
		}
		if code != "" {
			return &HTTPError{Status: samanthaStatus(code), Operation: operation, Code: code}
		}
	}
	return nil
}

func samanthaStatus(code string) int {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "401", "403", "710012001", "auth_required", "credential_expired", "gateway-error":
		return http.StatusUnauthorized
	case "429", "710022002", "710022004", "rate_limited":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

type completionEntry struct {
	EventData map[string]any
	Message   map[string]any
}

// completionMessages returns the decoded (event_data, message) of CMPL events.
func completionMessages(events []samanthaEvent) []completionEntry {
	result := make([]completionEntry, 0)
	for _, event := range events {
		if intAny(event.Data["event_type"]) != eventCompletion {
			continue
		}
		eventData, _ := decodeAnyJSON(event.Data["event_data"]).(map[string]any)
		if eventData == nil {
			continue
		}
		message, _ := decodeAnyJSON(eventData["message"]).(map[string]any)
		result = append(result, completionEntry{EventData: eventData, Message: message})
	}
	return result
}

func messageContent(message map[string]any, contentType int) (map[string]any, bool) {
	if message == nil || intAny(message["content_type"]) != contentType {
		return nil, false
	}
	content, ok := decodeAnyJSON(message["content"]).(map[string]any)
	return content, ok
}

func (c *Client) generateImage(ctx context.Context, credentials map[string]string, input ports.CapabilityInput) ([]map[string]any, error) {
	events, err := c.samanthaRequest(ctx, credentials, samanthaCompletionPath, samanthaPayload(input, skillImage, contentTypeImageInput), imageRequestTimeout, false, "image")
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0)
	for _, entry := range completionMessages(events) {
		content, ok := messageContent(entry.Message, contentTypeImageResult)
		if !ok {
			continue
		}
		for _, item := range listAny(content["data"]) {
			if image := imageItem(item); image != nil {
				result = append(result, image)
			}
		}
	}
	return result, nil
}

// imageItem picks one URL per generated image, preferring the raw original.
func imageItem(item map[string]any) map[string]any {
	containers := []map[string]any{item}
	if nested, ok := item["image"].(map[string]any); ok {
		containers = append(containers, nested)
	}
	for _, container := range containers {
		url, field, clean := pickMediaURL(container, rawImageFields, watermarkedImageFields)
		if url == "" {
			continue
		}
		value := map[string]any{"url": url, "source_variant": field}
		if clean {
			value["watermark_free"] = true
			value["watermark_method"] = "upstream_raw"
		}
		if variant, ok := container[field].(map[string]any); ok {
			if width := intAny(variant["width"]); width > 0 {
				value["width"] = width
			}
			if height := intAny(variant["height"]); height > 0 {
				value["height"] = height
			}
		}
		return value
	}
	return nil
}

func pickMediaURL(container map[string]any, rawFields, fallbackFields []string) (string, string, bool) {
	for _, field := range rawFields {
		if url := mediaURLOf(container[field]); url != "" {
			return url, field, true
		}
	}
	for _, field := range fallbackFields {
		if url := mediaURLOf(container[field]); url != "" {
			return url, field, false
		}
	}
	return "", "", false
}

// mediaURLOf reads a URL from a bare string or a {"url"|"main": ...} object.
func mediaURLOf(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return firstString(typed, "url", "main")
	}
	return ""
}

func (c *Client) generateVideo(ctx context.Context, credentials map[string]string, input ports.CapabilityInput) ([]map[string]any, error) {
	events, err := c.samanthaRequest(ctx, credentials, samanthaCompletionPath, samanthaPayload(input, skillVideo, contentTypeVideoInput), videoSubmitTimeout, false, "video")
	if err != nil {
		return nil, err
	}
	if taskID := asyncTaskID(events); taskID != "" {
		polled, err := c.samanthaRequest(ctx, credentials, samanthaAsyncStreamPath, map[string]any{"task_id": taskID, "event_id": 0}, videoPollTimeout, true, "video")
		if err != nil {
			return nil, err
		}
		return videoItems(polled), nil
	}
	if text := completionText(events); strings.Contains(text, "服务过载") || strings.Contains(text, "重试") {
		return nil, &HTTPError{Status: http.StatusServiceUnavailable, Operation: "video", Code: "overloaded"}
	}
	return videoItems(events), nil
}

// asyncTaskID returns fin_reason.async_task.id when fin_reason.reason == 1.
func asyncTaskID(events []samanthaEvent) string {
	for _, entry := range completionMessages(events) {
		finReason, _ := entry.EventData["fin_reason"].(map[string]any)
		if finReason == nil || intAny(finReason["reason"]) != 1 {
			continue
		}
		task, _ := finReason["async_task"].(map[string]any)
		if id := stringAny(task["id"]); id != "" {
			return id
		}
	}
	return ""
}

func completionText(events []samanthaEvent) string {
	var builder strings.Builder
	for _, entry := range completionMessages(events) {
		if content, ok := messageContent(entry.Message, contentTypeText); ok {
			text, _ := content["text"].(string)
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func videoItems(events []samanthaEvent) []map[string]any {
	result := make([]map[string]any, 0)
	for _, entry := range completionMessages(events) {
		content, ok := messageContent(entry.Message, contentTypeVideoResult)
		if !ok {
			continue
		}
		items := listAny(content["data"])
		if _, hasData := content["data"]; !hasData {
			items = []map[string]any{content}
		}
		for _, item := range items {
			url, field, clean := pickMediaURL(item, rawVideoFields, watermarkedVideoFields)
			if url == "" {
				url, field = videoModelURL(item["video_model"]), "video_model"
			}
			if url == "" {
				continue
			}
			value := map[string]any{"url": url, "video_url": url, "source_variant": field}
			if clean {
				value["watermark_free"] = true
				value["watermark_method"] = "upstream_raw"
			}
			if cover, ok := item["cover"].(map[string]any); ok {
				if coverURL, _, _ := pickMediaURL(cover, rawImageFields, append(watermarkedImageFields, "url")); coverURL != "" {
					value["cover_url"] = coverURL
				}
			} else if coverURL, _ := item["cover_url"].(string); coverURL != "" {
				value["cover_url"] = coverURL
			}
			result = append(result, value)
		}
	}
	return result
}

// videoModelURL decodes video_model.video_list.*.main_url (base64).
func videoModelURL(value any) string {
	model, _ := decodeAnyJSON(value).(map[string]any)
	list, _ := model["video_list"].(map[string]any)
	for _, raw := range list {
		info, _ := raw.(map[string]any)
		encoded, _ := info["main_url"].(string)
		if encoded == "" {
			continue
		}
		if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(decoded) > 0 {
			return string(decoded)
		}
	}
	return ""
}
