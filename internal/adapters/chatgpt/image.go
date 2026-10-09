package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

var dataImagePattern = regexp.MustCompile(`(?i)^data:([^;]+);base64,(.*)$`)

type uploadedImage struct {
	FileID   string
	FileName string
	FileSize int
	MIME     string
	Width    int
	Height   int
}

func (c *Client) imageCapability(ctx context.Context, input ports.CapabilityInput, token string, credentials map[string]string) (map[string]any, error) {
	if strings.TrimSpace(input.Prompt) == "" || strings.TrimSpace(input.Model) == "" {
		return nil, fmt.Errorf("image prompt and model are required")
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
		"system_hints": []string{"picture_v2"}, "partial_query": map[string]any{
			"id": fmt.Sprintf("query_%d", time.Now().UnixNano()), "author": map[string]string{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []string{input.Prompt}},
		}, "supports_buffering": true, "supported_encodings": []string{"v1"},
		"attachment_mime_types": imageMIMEs(images),
	}
	prepared, err := c.jsonRequest(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", token, prepare, nil)
	if err != nil {
		return nil, err
	}
	conduit := stringValue(prepared["conduit_token"])
	if conduit == "" {
		return nil, &SentinelError{Reason: "image_conduit"}
	}
	requirements, err := c.Requirements(ctx, credentials, token)
	if err != nil {
		return nil, err
	}
	extra := sentinelHeaders(conduit, requirements)
	parts := make([]any, 0, len(images)+1)
	for _, item := range images {
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + item.FileID, "size_bytes": item.FileSize, "width": item.Width, "height": item.Height})
	}
	parts = append(parts, input.Prompt)
	attachments := make([]any, 0, len(images))
	for _, item := range images {
		attachments = append(attachments, map[string]any{"id": item.FileID, "mimeType": item.MIME, "name": item.FileName, "size": item.FileSize, "width": item.Width, "height": item.Height})
	}
	message := map[string]any{"id": fmt.Sprintf("message_%d", time.Now().UnixNano()), "author": map[string]string{"role": "user"}, "create_time": float64(time.Now().Unix()), "content": map[string]any{"content_type": map[bool]string{true: "multimodal_text", false: "text"}[len(images) > 0], "parts": parts}, "metadata": map[string]any{"system_hints": []string{"picture_v2"}, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}, "attachments": attachments}}
	body := map[string]any{"action": "next", "messages": []any{message}, "parent_message_id": "client-created-root", "model": input.Model, "client_prepare_state": "sent", "timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]string{"kind": "primary_assistant"}, "enable_message_followups": true, "supports_buffering": true, "supported_encodings": []string{"v1"}, "system_hints": []string{"picture_v2"}, "client_contextual_info": map[string]string{"app_name": "chatgpt.com"}, "thinking_effort": "high"}
	response, err := c.rawJSONOrSSE(ctx, http.MethodPost, "/backend-api/conversation", token, body, extra)
	if err != nil {
		return nil, err
	}
	conversationID := parseConversationID(response)
	fileIDs, attachmentIDs := imageReferences(response)
	for attempt := 0; attempt < 8 && conversationID != "" && len(fileIDs)+len(attachmentIDs) == 0; attempt++ {
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		document, status, err := c.request(ctx, http.MethodGet, "/backend-api/conversation/"+conversationID, nil, token)
		if err != nil {
			return nil, err
		}
		if status >= 200 && status < 300 {
			fileIDs, attachmentIDs = imageResultReferences(document)
		}
	}
	data := make([]map[string]string, 0)
	for _, fileID := range fileIDs {
		if content, ok := c.downloadImage(ctx, conversationID, fileID, false, token); ok {
			data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(content)})
		}
	}
	for _, fileID := range attachmentIDs {
		if content, ok := c.downloadImage(ctx, conversationID, fileID, true, token); ok {
			data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(content)})
		}
	}
	if len(data) == 0 {
		return nil, &SentinelError{Reason: "image_result_missing"}
	}
	return map[string]any{"created": time.Now().Unix(), "data": data}, nil
}

func (c *Client) uploadImage(ctx context.Context, token, value string, index int) (uploadedImage, error) {
	match := dataImagePattern.FindStringSubmatch(strings.TrimSpace(value))
	mime := "image/png"
	encoded := strings.TrimSpace(value)
	if len(match) == 3 {
		mime, encoded = strings.ToLower(match[1]), strings.TrimSpace(match[2])
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return uploadedImage{}, fmt.Errorf("image input is invalid base64")
	}
	width, height := 1, 1
	if decoded, _, decodeErr := image.Decode(bytes.NewReader(data)); decodeErr == nil {
		width, height = decoded.Bounds().Dx(), decoded.Bounds().Dy()
	}
	ext := filepath.Ext("image_" + fmt.Sprintf("%d", index) + mimeExtension(mime))
	fileName := "image_" + fmt.Sprintf("%d", index) + ext
	created, err := c.jsonRequest(ctx, http.MethodPost, "/backend-api/files", token, map[string]any{"file_name": fileName, "file_size": len(data), "use_case": "multimodal", "timezone_offset_min": -480, "reset_rate_limits": false, "store_in_library": true, "library_persistence_mode": "opportunistic"}, nil)
	if err != nil {
		return uploadedImage{}, err
	}
	uploadURL, fileID := stringValue(created["upload_url"]), stringValue(created["file_id"])
	if uploadURL == "" || fileID == "" {
		return uploadedImage{}, &SentinelError{Reason: "image_upload_metadata"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, strings.NewReader(string(data)))
	if err != nil {
		return uploadedImage{}, err
	}
	request.Header.Set("Content-Type", mime)
	request.Header.Set("x-ms-blob-type", "BlockBlob")
	request.Header.Set("x-ms-version", "2020-04-08")
	request.Header.Set("Origin", c.BaseURL)
	request.Header.Set("Referer", c.BaseURL+"/")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return uploadedImage{}, portsTransportError(err)
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return uploadedImage{}, &HTTPError{Status: response.StatusCode, Operation: "image_upload"}
	}
	if _, status, err := c.request(ctx, http.MethodPost, "/backend-api/files/"+url.PathEscape(fileID)+"/uploaded", []byte("{}"), token); err != nil || status < 200 || status >= 300 {
		return uploadedImage{}, &HTTPError{Status: status, Operation: "image_upload_complete"}
	}
	return uploadedImage{FileID: fileID, FileName: fileName, FileSize: len(data), MIME: mime, Width: width, Height: height}, nil
}

func (c *Client) jsonRequest(ctx context.Context, method, path, token string, payload map[string]any, extra map[string]string) (map[string]any, error) {
	encoded, _ := json.Marshal(payload)
	body, status, err := c.requestWithHeaders(ctx, method, path, encoded, token, extra)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{Status: status, Operation: "chatgpt_json"}
	}
	var result map[string]any
	if json.Unmarshal(body, &result) != nil {
		return nil, &SentinelError{Reason: "chatgpt_json_decode"}
	}
	return result, nil
}

func (c *Client) rawJSONOrSSE(ctx context.Context, method, path, token string, payload map[string]any, extra map[string]string) ([]byte, error) {
	encoded, _ := json.Marshal(payload)
	body, status, err := c.requestWithHeaders(ctx, method, path, encoded, token, extra)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{Status: status, Operation: "chatgpt_conversation"}
	}
	return body, nil
}

func sentinelHeaders(conduit string, requirements SentinelRequirements) map[string]string {
	result := map[string]string{"X-Conduit-Token": conduit, "OpenAI-Sentinel-Chat-Requirements-Token": requirements.Token}
	if requirements.Proof != "" {
		result["OpenAI-Sentinel-Proof-Token"] = requirements.Proof
	}
	if requirements.Turnstile != "" {
		result["OpenAI-Sentinel-Turnstile-Token"] = requirements.Turnstile
	}
	if requirements.SOToken != "" {
		result["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}
	return result
}
func imageMIMEs(values []uploadedImage) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.MIME)
	}
	return result
}
func mimeExtension(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
func imageReferences(raw []byte) ([]string, []string) {
	files, attachments := []string{}, []string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var value any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &value) == nil {
			walkReferences(value, false, false, &files, &attachments)
		}
	}
	return files, attachments
}
func imageResultReferences(raw []byte) ([]string, []string) { return referencesFromValue(raw, true) }
func referencesFromValue(raw []byte, assistantOnly bool) ([]string, []string) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, nil
	}
	files, attachments := []string{}, []string{}
	walkReferences(value, assistantOnly, false, &files, &attachments)
	return files, attachments
}
func walkReferences(value any, assistantOnly, assistant bool, files, attachments *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if author, ok := typed["author"].(map[string]any); ok {
			assistant = stringValue(author["role"]) == "assistant" || stringValue(author["role"]) == "tool"
		}
		for key, child := range typed {
			if assistantOnly && key == "message" && !assistant {
				continue
			}
			walkReferences(child, assistantOnly, assistant, files, attachments)
		}
	case []any:
		for _, child := range typed {
			walkReferences(child, assistantOnly, assistant, files, attachments)
		}
	case string:
		for _, prefix := range []string{"file-service://", "sediment://"} {
			if index := strings.Index(typed, prefix); index >= 0 {
				id := strings.FieldsFunc(typed[index+len(prefix):], func(r rune) bool { return r == '"' || r == '\'' || r == '}' || r == ']' || r == ',' })
				if len(id) > 0 {
					if prefix == "file-service://" {
						*files = appendUnique(*files, id[0])
					} else {
						*attachments = appendUnique(*attachments, id[0])
					}
				}
			}
		}
	}
}
func appendUnique(values []string, value string) []string {
	for _, item := range values {
		if item == value {
			return values
		}
	}
	return append(values, value)
}
func (c *Client) downloadImage(ctx context.Context, conversationID, fileID string, attachment bool, token string) ([]byte, bool) {
	paths := []string{}
	if attachment {
		paths = append(paths, "/backend-api/conversation/"+conversationID+"/attachment/"+fileID+"/download")
	}
	paths = append(paths, "/backend-api/files/download/"+fileID, "/backend-api/files/"+fileID+"/download")
	for _, path := range paths {
		body, status, err := c.request(ctx, http.MethodGet, path, nil, token)
		if err == nil && status >= 200 && status < 300 && len(body) > 0 {
			return body, true
		}
	}
	return nil, false
}
