package doubao

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type CredentialResolver func(context.Context, string) (map[string]string, error)
type HTTPError struct {
	Status    int
	Operation string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Doubao %s endpoint returned HTTP %d", e.Operation, e.Status)
}
func (e *HTTPError) StatusCode() int { return e.Status }
func (e *HTTPError) Kind() string {
	if e.Status == 401 {
		return "credential_rejected"
	}
	if e.Status == 429 {
		return "upstream_rate_limited"
	}
	if e.Status >= 500 {
		return "upstream_unavailable"
	}
	return "upstream_error"
}

type Client struct {
	BaseURL           string
	HTTPClient        *http.Client
	ResolveCredential CredentialResolver
}

func NewClient(baseURL string, resolver CredentialResolver) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTPClient: &http.Client{Timeout: 60 * time.Second}, ResolveCredential: resolver}
}

func (c *Client) Models(ctx context.Context) ([]ports.ModelDescriptor, error) {
	return c.models(ctx, nil)
}

func (c *Client) ModelsWithAccount(ctx context.Context, accountID string) ([]ports.ModelDescriptor, error) {
	if c.ResolveCredential == nil {
		return nil, fmt.Errorf("Doubao credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if credentialLookup(credentials, "Cookie", "cookie") == "" {
		return nil, fmt.Errorf("Doubao credential is unavailable")
	}
	return c.models(ctx, credentials)
}

func (c *Client) models(ctx context.Context, credentials map[string]string) ([]ports.ModelDescriptor, error) {
	requestBody := []byte(mustJSON(map[string]string{
		"bot_id":        credentialLookup(credentials, "bot_id"),
		"language_code": "zh",
	}))
	if string(requestBody) == `{"bot_id":"","language_code":"zh"}` {
		requestBody = []byte(mustJSON(map[string]string{"bot_id": "7338286299411103781", "language_code": "zh"}))
	}
	body, status, err := c.requestWithCredentials(ctx, http.MethodPost, "/alice/slot/action_bar_v3/brief_list", requestBody, credentials)
	if err != nil {
		return nil, err
	}
	if status >= 200 && status < 300 {
		var payload any
		if json.Unmarshal(body, &payload) == nil {
			models := parseModels(payload)
			if len(models) > 0 {
				return models, nil
			}
		}
		if models, fallbackErr := c.modelsFromChatPage(ctx, credentials); fallbackErr == nil && len(models) > 0 {
			return models, nil
		}
		return nil, fmt.Errorf("Doubao model response is invalid")
	}
	if status == http.StatusNotFound {
		if models, fallbackErr := c.modelsFromChatPage(ctx, credentials); fallbackErr == nil && len(models) > 0 {
			return models, nil
		}
	}
	return nil, &HTTPError{Status: status, Operation: "models"}
}

func (c *Client) modelsFromChatPage(ctx context.Context, credentials map[string]string) ([]ports.ModelDescriptor, error) {
	body, status, err := c.requestWithCredentials(ctx, http.MethodGet, "/chat/", nil, credentials)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{Status: status, Operation: "models"}
	}
	value, err := parseRouterData(body)
	if err != nil {
		return nil, err
	}
	return parseModels(value), nil
}

func (c *Client) Chat(ctx context.Context, input ports.ChatInput, accountID string) (ports.ChatResult, error) {
	var result ports.ChatResult
	err := c.ChatStream(ctx, input, accountID, func(chunk ports.StreamChunk) error {
		if result.ID == "" {
			result.ID = chunk.ID
		}
		if chunk.CreatedAt != 0 {
			result.CreatedAt = chunk.CreatedAt
		}
		result.Text += chunk.Text
		if chunk.FinishReason != "" {
			result.FinishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		return nil
	})
	if err != nil {
		return ports.ChatResult{}, err
	}
	if result.FinishReason == "" {
		result.FinishReason = "stop"
	}
	return result, nil
}

func (c *Client) ChatStream(ctx context.Context, input ports.ChatInput, accountID string, emit func(ports.StreamChunk) error) error {
	if c.ResolveCredential == nil {
		return fmt.Errorf("Doubao credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return err
	}
	if credentialValue(credentials, "Cookie", "cookie") == "" {
		return fmt.Errorf("Doubao credential is unavailable")
	}
	prompt := ""
	for _, message := range input.Messages {
		if message.Content != "" {
			prompt = message.Content
		}
	}
	alice := map[string]any{"event_type": 1, "message": map[string]any{"conversation_id": "0", "section_id": "0", "local_message_id": fmt.Sprintf("%d", time.Now().UnixNano()), "content_type": 1, "content": mustJSON(map[string]string{"text": prompt}), "reply_id": "", "ext": map[string]string{"origin": c.BaseURL, "stream": "1", "answer_with_suggest": "1", "browser_language": "zh-CN"}, "local_conversation_id": "0", "bot_id": "7338286299411103781", "meta_infos": []any{}}}
	encoded, _ := json.Marshal(alice)
	body, _ := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(encoded)})
	raw, status, err := c.requestWithCredentials(ctx, http.MethodPost, "/alice/message/stream_call_bot", body, credentials)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &HTTPError{Status: status, Operation: "chat"}
	}
	return parseSSE(raw, emit)
}

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
	if strings.TrimSpace(credentials["cookie"]) == "" && strings.TrimSpace(credentials["Cookie"]) == "" {
		return ports.CapabilityResult{}, fmt.Errorf("Doubao credential is unavailable")
	}
	skillID, contentType := 3, 2009
	if input.Capability == "video" {
		skillID, contentType = 17, 2020
	}
	payload := samanthaPayload(input, skillID, contentType)
	raw, status, err := c.samanthaRequest(ctx, credentials, payload, 3*time.Minute)
	if err != nil {
		return ports.CapabilityResult{}, err
	}
	if status < 200 || status >= 300 {
		return ports.CapabilityResult{}, &HTTPError{Status: status, Operation: input.Capability}
	}
	if code, _ := samanthaError(raw); code != "" {
		return ports.CapabilityResult{}, &HTTPError{Status: samanthaStatus(code), Operation: input.Capability}
	}
	items := extractSamanthaMedia(raw, input.Capability)
	if len(items) == 0 {
		return ports.CapabilityResult{}, fmt.Errorf("Doubao returned no %s result", input.Capability)
	}
	return ports.CapabilityResult{StatusCode: http.StatusOK, Body: map[string]any{"created": time.Now().Unix(), "data": items}}, nil
}

func samanthaPayload(input ports.CapabilityInput, skillID, contentType int) map[string]any {
	content := map[string]any{"text": input.Prompt}
	if input.Ratio != "" {
		content["ratio"] = input.Ratio
	}
	if input.Model != "" {
		content["model"] = input.Model
	}
	if input.Duration != nil {
		content["duration"] = *input.Duration
	}
	variables := map[string]any{"style": "", "ratio": input.Ratio, "model": input.Model, "template_type": "placeholder"}
	if input.Duration != nil {
		variables["duration"] = fmt.Sprintf("%d", *input.Duration)
	}
	inputSkill := map[string]any{"skill_id": fmt.Sprintf("%d", skillID), "skill_type": skillID, "variables": variables}
	contextJSON, _ := json.Marshal(map[string]any{"query_context": variables})
	inputSkillJSON, _ := json.Marshal(inputSkill)
	extra := map[string]any{"input_skill": string(inputSkillJSON), "answer_with_suggest": "0", "samantha_context": string(contextJSON)}
	return map[string]any{
		"messages":          []any{map[string]any{"content": mustJSON(content), "content_type": contentType, "attachments": []any{}, "references": []any{}, "skill": map[string]any{"skill_type": skillID, "skill_type_no_default": skillID, "skill_id": fmt.Sprintf("%d", skillID), "skill_id_no_default": fmt.Sprintf("%d", skillID)}, "ext": map[string]any{"samantha_context": string(contextJSON)}, "extra_ext": extra, "extraExt": extra}},
		"completion_option": map[string]any{"is_regen": false, "with_suggest": true, "need_create_conversation": true, "launch_stage": 1, "is_replace": false, "is_delete": false, "is_ai_playground": false, "memory_type": 2, "message_from": 0, "use_deep_think": false, "use_auto_cot": false, "resend_for_regen": false, "enable_commerce_credit": false, "action_bar_skill_id": skillID},
		"evaluate_option":   map[string]any{"web_ab_params": ""}, "local_conversation_id": fmt.Sprintf("local_%d", time.Now().UnixNano()), "local_message_id": fmt.Sprintf("message_%d", time.Now().UnixNano()),
	}
}

func (c *Client) samanthaRequest(ctx context.Context, credentials map[string]string, payload map[string]any, timeout time.Duration) ([]byte, int, error) {
	query := url.Values{"aid": {"582478"}, "real_aid": {"582478"}, "device_id": {credentialValue(credentials, "device_id", "714003710229497")}, "tea_uuid": {credentialValue(credentials, "device_id", "714003710229497")}, "web_id": {credentialValue(credentials, "web_id", "7604137868021548590")}, "device_platform": {"web"}, "language": {"zh"}, "region": {"CN"}, "sys_region": {"CN"}, "pkg_type": {"release_version"}, "version_code": {"20800"}, "pc_version": {"2.1.7"}, "chromium_version": {"131.0.0.0"}, "client_platform": {"pc_client"}, "runtime": {"web"}, "runtime_version": {"3.5.4"}, "samantha_web": {"1"}, "use-olympus-account": {"1"}, "fp": {credentialValue(credentials, "fp", "verify_mlcfw5f7_TPq0YmFD_NrsC_4RuQ_BJPg_M5W7i58I7wV0")}, "web_tab_id": {fmt.Sprintf("tab_%d", time.Now().UnixNano())}}
	if token := credentialValue(credentials, "msToken", ""); token != "" {
		query.Set("msToken", token)
	}
	encoded, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/samantha/chat/completion?"+query.Encode(), bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", credentialValue(credentials, "User-Agent", "Mozilla/5.0"))
	request.Header.Set("Origin", credentialValue(credentials, "Origin", c.BaseURL))
	request.Header.Set("Referer", credentialValue(credentials, "Referer", c.BaseURL+"/chat/"))
	request.Header.Set("Cookie", credentialValue(credentials, "Cookie", credentials["cookie"]))
	client := *c.HTTPClient
	client.Timeout = timeout
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, ports.NewTransportError("POST /samantha/chat/completion", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	return body, response.StatusCode, err
}

func credentialValue(values map[string]string, name, fallback string) string {
	if value := strings.TrimSpace(values[name]); value != "" {
		return value
	}
	return fallback
}

func credentialLookup(values map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(values[name]); value != "" {
			return value
		}
	}
	return ""
}

func samanthaError(raw []byte) (string, string) {
	for _, event := range samanthaEvents(raw) {
		if nested, ok := decodeAnyJSON(event["event_data"]).(map[string]any); ok {
			if code := stringAny(nested["code"], nested["error_code"]); code != "" && code != "0" {
				return code, stringAny(nested["message"], nested["msg"], nested["error_msg"])
			}
		}
		if code := stringAny(event["code"], event["error_code"]); code != "" && code != "0" {
			return code, stringAny(event["message"], event["msg"], event["error_msg"])
		}
	}
	return "", ""
}

func samanthaStatus(code string) int {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "401", "403", "710012001", "auth_required", "credential_expired":
		return http.StatusUnauthorized
	case "429", "710022002", "710022004", "rate_limited":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func samanthaEvents(raw []byte) []map[string]any {
	result := make([]map[string]any, 0)
	for _, frame := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n") {
		data := make([]string, 0)
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) == 0 || strings.Join(data, "\n") == "[DONE]" {
			continue
		}
		var value map[string]any
		if json.Unmarshal([]byte(strings.Join(data, "\n")), &value) == nil {
			result = append(result, value)
		}
	}
	return result
}

func extractSamanthaMedia(raw []byte, capability string) []map[string]any {
	contentType := 2010
	if capability == "video" {
		contentType = 2021
	}
	result := make([]map[string]any, 0)
	for _, event := range samanthaEvents(raw) {
		message, _ := event["message"].(map[string]any)
		if data := decodeAnyJSON(event["event_data"]); data != nil {
			if nested, ok := data.(map[string]any); ok {
				message, _ = nested["message"].(map[string]any)
			}
		}
		if message == nil {
			continue
		}
		if intAny(message["content_type"]) != contentType {
			continue
		}
		content := decodeAnyJSON(message["content"])
		walkMedia(content, capability, &result)
	}
	return result
}

func walkMedia(value any, capability string, result *[]map[string]any) {
	if object, ok := value.(map[string]any); ok {
		keys := []string{"url"}
		if capability == "image" {
			keys = []string{"image_ori_raw", "image_raw", "image_ori", "image_thumb", "url"}
		} else {
			keys = []string{"video_url", "main_url", "url"}
		}
		for _, key := range keys {
			if text, ok := object[key].(string); ok && (strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://")) {
				*result = append(*result, map[string]any{"url": text, "video_url": func() string {
					if capability == "video" {
						return text
					}
					return ""
				}()})
				return
			}
		}
		for _, child := range object {
			walkMedia(child, capability, result)
		}
	} else if list, ok := value.([]any); ok {
		for _, child := range list {
			walkMedia(child, capability, result)
		}
	}
}

func decodeAnyJSON(value any) any {
	if text, ok := value.(string); ok && strings.HasPrefix(strings.TrimSpace(text), "{") {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			return parsed
		}
	}
	return value
}
func stringAny(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case float64:
			return fmt.Sprintf("%.0f", typed)
		}
	}
	return ""
}
func intAny(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		var result int
		_, _ = fmt.Sscan(typed, &result)
		return result
	}
	return 0
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, cookie string) ([]byte, int, error) {
	credentials := map[string]string{}
	if cookie != "" {
		credentials["Cookie"] = cookie
	}
	return c.requestWithCredentials(ctx, method, path, body, credentials)
}

func (c *Client) requestWithCredentials(ctx context.Context, method, path string, body []byte, credentials map[string]string) ([]byte, int, error) {
	target := c.BaseURL + path
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, 0, err
	}
	query := parsed.Query()
	for key, value := range doubaoQuery(credentials) {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", credentialLookup(credentials, "User-Agent", "user_agent", "user-agent"))
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0")
	}
	if cookie := credentialLookup(credentials, "Cookie", "cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	origin := credentialLookup(credentials, "Origin", "origin")
	if origin == "" {
		origin = c.BaseURL
	}
	req.Header.Set("Origin", origin)
	referer := credentialLookup(credentials, "Referer", "referer")
	if referer == "" {
		referer = c.BaseURL + "/chat/"
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("agw-js-conv", "str")
	if csrf := doubaoCSRF(credentialLookup(credentials, "Cookie", "cookie")); csrf != "" {
		req.Header.Set("x-tt-passport-csrf-token", csrf)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, ports.NewTransportError(method+" "+path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, err
}

func doubaoQuery(credentials map[string]string) map[string]string {
	deviceID := credentialLookup(credentials, "device_id")
	if deviceID == "" {
		deviceID = "714003710229497"
	}
	webID := credentialLookup(credentials, "web_id")
	if webID == "" {
		webID = "7604137868021548590"
	}
	fp := credentialLookup(credentials, "fp")
	if fp == "" {
		fp = "verify_mlcfw5f7_TPq0YmFD_NrsC_4RuQ_BJPg_M5W7i58I7wV0"
	}
	values := map[string]string{
		"aid": "582478", "real_aid": "582478", "device_id": deviceID, "tea_uuid": deviceID,
		"web_id": webID, "device_platform": "web", "language": "zh", "region": "CN",
		"sys_region": "CN", "pkg_type": "release_version", "version_code": "20800",
		"pc_version": "2.1.7", "chromium_version": "131.0.0.0", "client_platform": "pc_client",
		"runtime": "web", "runtime_version": "3.5.4", "samantha_web": "1",
		"use-olympus-account": "1", "fp": fp, "web_tab_id": fmt.Sprintf("tab_%d", time.Now().UnixNano()),
	}
	if token := credentialLookup(credentials, "msToken", "mstoken"); token != "" {
		values["msToken"] = token
	}
	return values
}

func doubaoCSRF(cookie string) string {
	for _, item := range strings.Split(cookie, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		if ok && (name == "passport_csrf_token" || name == "passport_csrf_token_default") {
			return value
		}
	}
	return ""
}

func parseModels(value any) []ports.ModelDescriptor {
	result := make([]ports.ModelDescriptor, 0)
	seen := map[string]bool{}
	collectDoubaoModels(value, "", &result, seen)
	return result
}

func collectDoubaoModels(value any, modeID string, result *[]ports.ModelDescriptor, seen map[string]bool) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			collectDoubaoModels(child, modeID, result, seen)
		}
	case map[string]any:
		if typed["need_login"] != true {
			id := firstString(typed, "model_item_key", "modelItemKey", "model_id", "id", "item_id")
			if id != "" && !seen[id] {
				if subscription, ok := typed["subscribe_config"].(map[string]any); !ok || subscription["need_upgrade"] != true {
					seen[id] = true
					name := firstString(typed, "name", "display_name")
					if name == "" {
						name = id
					}
					*result = append(*result, ports.ModelDescriptor{UpstreamID: id, DisplayName: name, Capabilities: []string{"chat"}})
				}
			}
		}
		for _, key := range []string{"mode_list", "modeList"} {
			if modes, ok := typed[key].(map[string]any); ok {
				if items, ok := modes["item_list"].([]any); ok {
					for _, item := range items {
						mode := modeID
						if object, ok := item.(map[string]any); ok {
							mode = firstString(object, "mode_id", "modeId")
						}
						collectDoubaoModels(item, mode, result, seen)
					}
				}
			}
		}
		for _, key := range []string{"model_list", "modelList"} {
			if models, ok := typed[key].(map[string]any); ok {
				if items, ok := models["item_list"].([]any); ok {
					for _, raw := range items {
						object, ok := raw.(map[string]any)
						if !ok || object["need_login"] == true {
							continue
						}
						id := firstString(object, "model_item_key", "modelItemKey", "model_id", "id", "item_id")
						if id == "" || seen[id] {
							continue
						}
						if subscription, ok := object["subscribe_config"].(map[string]any); ok && subscription["need_upgrade"] == true {
							continue
						}
						seen[id] = true
						name := firstString(object, "name", "display_name")
						if name == "" {
							name = id
						}
						_ = modeID
						*result = append(*result, ports.ModelDescriptor{UpstreamID: id, DisplayName: name, Capabilities: []string{"chat"}})
					}
				}
			}
		}
		for key, child := range typed {
			if key == "mode_list" || key == "modeList" || key == "model_list" || key == "modelList" {
				continue
			}
			collectDoubaoModels(child, modeID, result, seen)
		}
	}
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseRouterData(body []byte) (any, error) {
	text := string(body)
	marker := "window._ROUTER_DATA ="
	index := strings.Index(text, marker)
	if index < 0 {
		return nil, fmt.Errorf("Doubao model response is invalid")
	}
	start := strings.Index(text[index+len(marker):], "{")
	if start < 0 {
		return nil, fmt.Errorf("Doubao model response is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(text[index+len(marker)+start:]))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("Doubao model response is invalid")
	}
	return value, nil
}

func parseSSE(body []byte, emit func(ports.StreamChunk) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(raw), &event) != nil {
			continue
		}
		text := findText(event)
		if text != "" {
			if err := emit(ports.StreamChunk{ID: fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano()), Text: text, CreatedAt: time.Now().Unix()}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
func findText(value any) string {
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"text", "content", "answer"} {
			if text, ok := object[key].(string); ok && text != "" {
				return text
			}
		}
		for _, child := range object {
			if text := findText(child); text != "" {
				return text
			}
		}
	}
	if list, ok := value.([]any); ok {
		for _, child := range list {
			if text := findText(child); text != "" {
				return text
			}
		}
	}
	return ""
}
func mustJSON(value any) string { encoded, _ := json.Marshal(value); return string(encoded) }
