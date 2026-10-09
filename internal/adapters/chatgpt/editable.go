package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func (c *Client) editableCapability(ctx context.Context, input ports.CapabilityInput, token string, credentials map[string]string) (map[string]any, error) {
	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	if kind != "ppt" && kind != "psd" {
		return nil, fmt.Errorf("editable file kind is invalid")
	}
	if kind == "psd" && len(input.Images) == 0 {
		return nil, fmt.Errorf("PSD generation requires an image")
	}
	prompt := "请根据用户需求制作一个可以编辑的 PPT，直接输出 PPT 文件和素材 zip。"
	if kind == "psd" {
		prompt = "请把用户提供的海报拆分为可编辑图层，直接输出 PSD 文件和图层素材 zip。"
	}
	if strings.TrimSpace(input.Prompt) != "" {
		prompt += "\n\n用户补充需求：" + strings.TrimSpace(input.Prompt)
	}
	images := make([]uploadedImage, 0, len(input.Images))
	for index, value := range input.Images {
		item, err := c.uploadImage(ctx, token, value, index+1)
		if err != nil {
			return nil, err
		}
		images = append(images, item)
	}
	prepare := map[string]any{
		"action": "next", "fork_from_shared_post": false, "parent_message_id": "client-created-root",
		"model": input.Model, "client_prepare_state": "success", "timezone_offset_min": -480,
		"timezone": "Asia/Shanghai", "conversation_mode": map[string]string{"kind": "primary_assistant"},
		"partial_query":      map[string]any{"id": fmt.Sprintf("query_%d", time.Now().UnixNano()), "author": map[string]string{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}},
		"supports_buffering": true, "supported_encodings": []string{"v1"}, "attachment_mime_types": imageMIMEs(images), "thinking_effort": "high",
	}
	prepared, err := c.jsonRequest(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", token, prepare, nil)
	if err != nil {
		return nil, err
	}
	conduit := stringValue(prepared["conduit_token"])
	if conduit == "" {
		return nil, &SentinelError{Reason: "editable_conduit"}
	}
	requirements, err := c.Requirements(ctx, credentials, token)
	if err != nil {
		return nil, err
	}
	parts := make([]any, 0, len(images)+1)
	attachments := make([]any, 0, len(images))
	for _, item := range images {
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + item.FileID, "size_bytes": item.FileSize, "width": item.Width, "height": item.Height})
		attachments = append(attachments, map[string]any{"id": item.FileID, "size": item.FileSize, "name": item.FileName, "mime_type": item.MIME, "width": item.Width, "height": item.Height, "source": "library"})
	}
	parts = append(parts, prompt)
	message := map[string]any{"id": fmt.Sprintf("message_%d", time.Now().UnixNano()), "author": map[string]string{"role": "user"}, "create_time": float64(time.Now().Unix()), "content": map[string]any{"content_type": map[bool]string{true: "multimodal_text", false: "text"}[len(images) > 0], "parts": parts}, "metadata": map[string]any{"attachments": attachments}}
	body := map[string]any{"action": "next", "messages": []any{message}, "parent_message_id": "client-created-root", "model": input.Model, "client_prepare_state": "sent", "timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]string{"kind": "primary_assistant"}, "enable_message_followups": true, "supports_buffering": true, "supported_encodings": []string{"v1"}, "client_contextual_info": map[string]string{"app_name": "chatgpt.com"}, "thinking_effort": "high"}
	response, err := c.rawJSONOrSSE(ctx, http.MethodPost, "/backend-api/conversation", token, body, sentinelHeaders(conduit, requirements))
	if err != nil {
		return nil, err
	}
	conversationID := parseConversationID(response)
	if conversationID == "" {
		return nil, &SentinelError{Reason: "editable_conversation_id"}
	}
	artifacts := make([]map[string]string, 0)
	for attempt := 0; attempt < 12; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		documentBody, status, err := c.request(ctx, http.MethodGet, "/backend-api/conversation/"+conversationID, nil, token)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			continue
		}
		var document any
		if json.Unmarshal(documentBody, &document) != nil {
			continue
		}
		collectArtifacts(document, &artifacts)
		if hasEditableArtifacts(artifacts, kind) {
			break
		}
	}
	files := make([]map[string]any, 0)
	for _, artifact := range artifacts {
		content, ok := c.downloadImage(ctx, conversationID, artifact["file_id"], false, token)
		if ok {
			files = append(files, map[string]any{"name": artifact["name"], "mime_type": artifact["mime_type"], "content_base64": base64.StdEncoding.EncodeToString(content)})
		}
	}
	if len(files) == 0 {
		return nil, &SentinelError{Reason: "editable_artifacts_missing"}
	}
	return map[string]any{"conversation_id": conversationID, "kind": kind, "files": files}, nil
}

func collectArtifacts(value any, result *[]map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		fileID := stringValue(typed["file_id"])
		if fileID == "" {
			fileID = stringValue(typed["attachment_id"])
		}
		name := stringValue(typed["name"])
		if name == "" {
			name = stringValue(typed["file_name"])
		}
		if name == "" {
			name = stringValue(typed["filename"])
		}
		mime := strings.ToLower(stringValue(typed["mime_type"]))
		if mime == "" {
			mime = strings.ToLower(stringValue(typed["mimeType"]))
		}
		if fileID != "" && name != "" && (strings.Contains(strings.ToLower(name), ".ppt") || strings.HasSuffix(strings.ToLower(name), ".psd") || strings.HasSuffix(strings.ToLower(name), ".zip") || strings.Contains(mime, "powerpoint") || strings.Contains(mime, "photoshop")) {
			found := false
			for _, item := range *result {
				if item["file_id"] == fileID {
					found = true
					break
				}
			}
			if !found {
				*result = append(*result, map[string]string{"file_id": fileID, "name": name, "mime_type": mime})
			}
		}
		for _, child := range typed {
			collectArtifacts(child, result)
		}
	case []any:
		for _, child := range typed {
			collectArtifacts(child, result)
		}
	}
}

func hasEditableArtifacts(values []map[string]string, kind string) bool {
	primary := false
	zip := false
	for _, item := range values {
		name := strings.ToLower(item["name"])
		if kind == "ppt" && (strings.HasSuffix(name, ".ppt") || strings.HasSuffix(name, ".pptx")) {
			primary = true
		}
		if kind == "psd" && strings.HasSuffix(name, ".psd") {
			primary = true
		}
		if strings.HasSuffix(name, ".zip") {
			zip = true
		}
	}
	return primary && zip
}
