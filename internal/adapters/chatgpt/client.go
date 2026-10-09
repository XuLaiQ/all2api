package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/google/uuid"
)

type CredentialResolver func(context.Context, string) (map[string]string, error)
type HTTPError struct {
	Status    int
	Operation string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("ChatGPT %s endpoint returned HTTP %d", e.Operation, e.Status)
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
	HTTPClient        HTTPDoer
	ResolveCredential CredentialResolver
	SentinelEnabled   bool
	AuthMode          string
	UserAgent         string
	Impersonate       string
	DeviceID          string
	SessionID         string
	ChatGPTAccountID  string
	Fingerprint       map[string]string
	Timeout           time.Duration
}

const (
	chatGPTClientVersion    = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
	chatGPTBuildNumber      = "6708908"
	chatGPTDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
)

func NewClient(baseURL string, resolver CredentialResolver) *Client {
	return NewClientWithProxy(baseURL, "", resolver)
}

func NewClientWithProxy(baseURL, proxy string, resolver CredentialResolver) *Client {
	proxy = strings.TrimSpace(proxy)
	httpClient, err := newChatGPTHTTPClient(baseURL, proxy, 60*time.Second)
	if err != nil {
		fallback := &http.Client{Timeout: 60 * time.Second}
		if proxyURL, parseErr := url.Parse(proxy); parseErr == nil && proxyURL.Scheme != "" && proxyURL.Host != "" {
			fallback.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}
		httpClient = fallback
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), HTTPClient: httpClient, ResolveCredential: resolver,
		SentinelEnabled: true,
		AuthMode:        "web",
		UserAgent:       chatGPTDefaultUserAgent,
		Impersonate:     "chrome110",
		DeviceID:        uuid.NewString(), SessionID: uuid.NewString(),
		Fingerprint: defaultWebFingerprint(), Timeout: 60 * time.Second,
	}
}

func (c *Client) Models(ctx context.Context) ([]ports.ModelDescriptor, error) {
	return c.models(ctx, "")
}

func (c *Client) ModelsWithAccount(ctx context.Context, accountID string) ([]ports.ModelDescriptor, error) {
	if c.ResolveCredential == nil {
		return nil, fmt.Errorf("ChatGPT credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(credentials["access_token"])
	if token == "" {
		token = strings.TrimSpace(credentials["token"])
	}
	if token == "" {
		return nil, fmt.Errorf("ChatGPT credential is unavailable")
	}
	return c.withCredentials(credentials).models(ctx, token)
}

func (c *Client) models(ctx context.Context, token string) ([]ports.ModelDescriptor, error) {
	if _, _, err := c.bootstrap(ctx, token, c.UserAgent); err != nil {
		return nil, err
	}
	body, status, err := c.request(ctx, http.MethodGet, "/backend-api/models?history_and_training_disabled=false", nil, token)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{Status: status, Operation: "models"}
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return nil, fmt.Errorf("ChatGPT model response is invalid")
	}
	return parseModels(payload), nil
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
		return fmt.Errorf("ChatGPT credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(credentials["access_token"])
	if token == "" {
		token = strings.TrimSpace(credentials["token"])
	}
	if token == "" {
		return fmt.Errorf("ChatGPT credential is unavailable")
	}
	client := c.withCredentials(credentials)
	requirements := SentinelRequirements{}
	if c.SentinelEnabled {
		requirements, err = client.Requirements(ctx, credentials, token)
		if err != nil {
			return err
		}
	}
	messages := make([]map[string]any, 0, len(input.Messages))
	for _, message := range input.Messages {
		messages = append(messages, map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": message.Role}, "content": map[string]any{"content_type": "text", "parts": []string{message.Content}}})
	}
	payload := map[string]any{
		"action": "next", "messages": messages, "model": input.Model, "parent_message_id": uuid.NewString(),
		"conversation_mode": map[string]string{"kind": "primary_assistant"}, "conversation_origin": nil,
		"force_paragen": false, "force_paragen_model_slug": "", "force_rate_limit": false,
		"force_use_sse": true, "history_and_training_disabled": true, "reset_rate_limits": false,
		"suggestions": []any{}, "supported_encodings": []any{}, "system_hints": []any{},
		"timezone": "Asia/Shanghai", "timezone_offset_min": -480,
		"variant_purpose": "comparison_implicit", "websocket_request_id": uuid.NewString(),
		"client_contextual_info": map[string]any{
			"is_dark_mode": false, "time_since_loaded": 120, "page_height": 900, "page_width": 1400,
			"pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560,
		},
	}
	encoded, _ := json.Marshal(payload)
	extra := map[string]string{}
	if requirements.Token != "" {
		extra["OpenAI-Sentinel-Chat-Requirements-Token"] = requirements.Token
	}
	if requirements.Proof != "" {
		extra["OpenAI-Sentinel-Proof-Token"] = requirements.Proof
	}
	if requirements.Turnstile != "" {
		extra["OpenAI-Sentinel-Turnstile-Token"] = requirements.Turnstile
	}
	if requirements.SOToken != "" {
		extra["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}
	body, status, err := client.requestWithHeaders(ctx, http.MethodPost, "/backend-api/conversation", encoded, token, extra)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &HTTPError{Status: status, Operation: "conversation"}
	}
	return parseSSE(body, emit)
}

func (c *Client) withCredentials(credentials map[string]string) *Client {
	clone := *c
	clone.Fingerprint = cloneStringMap(c.Fingerprint)
	if clone.AuthMode == "" {
		clone.AuthMode = "web"
	}
	if value := credentialString(credentials, "auth_mode", "auth-mode", "authMode"); value != "" {
		clone.AuthMode = strings.ToLower(value)
	}
	if value := credentialString(credentials, "user_agent", "User-Agent", "user-agent"); value != "" {
		clone.UserAgent = value
	}
	if value := credentialString(credentials, "impersonate"); value != "" {
		clone.Impersonate = value
	}
	if value := credentialString(credentials, "oai_device_id", "oai-device-id", "device_id"); value != "" {
		clone.DeviceID = value
	}
	if value := credentialString(credentials, "oai_session_id", "oai-session-id", "session_id"); value != "" {
		clone.SessionID = value
	}
	if value := credentialString(credentials, "chatgpt_account_id", "chatgpt-account-id"); value != "" {
		clone.ChatGPTAccountID = value
	}
	applyCredentialFingerprint(&clone, credentials)
	if proxy := strings.TrimSpace(credentials["proxy"]); proxy != "" {
		clone.HTTPClient = cloneHTTPClient(c.HTTPClient, c.BaseURL, proxy, c.Timeout)
	}
	return &clone
}

func credentialString(values map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(values[name]); value != "" {
			return value
		}
	}
	return ""
}

func defaultWebFingerprint() map[string]string {
	return map[string]string{
		"sec-ch-ua":                   `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		"sec-ch-ua-arch":              `"x86"`,
		"sec-ch-ua-bitness":           `"64"`,
		"sec-ch-ua-full-version":      `"143.0.3650.96"`,
		"sec-ch-ua-full-version-list": `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`,
		"sec-ch-ua-mobile":            "?0",
		"sec-ch-ua-model":             `""`,
		"sec-ch-ua-platform":          `"Windows"`,
		"sec-ch-ua-platform-version":  `"19.0.0"`,
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func applyCredentialFingerprint(client *Client, credentials map[string]string) {
	if client.Fingerprint == nil {
		client.Fingerprint = defaultWebFingerprint()
	}
	for key, value := range credentials {
		applyFingerprintEntry(client, key, value)
	}
	if raw := credentialString(credentials, "fp", "fingerprint"); raw != "" {
		var nested map[string]any
		if json.Unmarshal([]byte(raw), &nested) == nil {
			for key, value := range nested {
				text, ok := value.(string)
				if !ok {
					continue
				}
				applyFingerprintEntry(client, key, text)
			}
		}
	}
}

func applyFingerprintEntry(client *Client, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(key, "_", "-")))
	switch normalized {
	case "user-agent":
		client.UserAgent = value
	case "impersonate":
		client.Impersonate = value
	case "oai-device-id", "device-id":
		client.DeviceID = value
	case "oai-session-id", "session-id":
		client.SessionID = value
	default:
		if _, ok := webFingerprintHeaders[normalized]; ok {
			client.Fingerprint[normalized] = value
		}
	}
}

var webFingerprintHeaders = map[string]string{
	"sec-ch-ua":                   "Sec-Ch-Ua",
	"sec-ch-ua-arch":              "Sec-Ch-Ua-Arch",
	"sec-ch-ua-bitness":           "Sec-Ch-Ua-Bitness",
	"sec-ch-ua-full-version":      "Sec-Ch-Ua-Full-Version",
	"sec-ch-ua-full-version-list": "Sec-Ch-Ua-Full-Version-List",
	"sec-ch-ua-mobile":            "Sec-Ch-Ua-Mobile",
	"sec-ch-ua-model":             "Sec-Ch-Ua-Model",
	"sec-ch-ua-platform":          "Sec-Ch-Ua-Platform",
	"sec-ch-ua-platform-version":  "Sec-Ch-Ua-Platform-Version",
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, token string) ([]byte, int, error) {
	return c.requestWithHeaders(ctx, method, path, body, token, nil)
}

func (c *Client) requestWithHeaders(ctx context.Context, method, path string, body []byte, token string, extra map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	c.setWebHeaders(req, path)
	req.Header.Set("Accept", "application/json")
	if strings.Contains(path, "conversation") {
		req.Header.Set("Accept", "text/event-stream")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range extra {
		req.Header.Set(key, value)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, ports.NewTransportError(method+" "+path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, err
}

func (c *Client) setWebHeaders(req *http.Request, path string) {
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Origin", c.BaseURL)
	req.Header.Set("Referer", c.BaseURL+"/")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Priority", "u=1, i")
	for key, header := range webFingerprintHeaders {
		if value := strings.TrimSpace(c.Fingerprint[key]); value != "" {
			req.Header.Set(header, value)
		}
	}
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("OAI-Device-Id", c.DeviceID)
	req.Header.Set("OAI-Session-Id", c.SessionID)
	req.Header.Set("OAI-Language", "zh-CN")
	req.Header.Set("OAI-Client-Version", chatGPTClientVersion)
	req.Header.Set("OAI-Client-Build-Number", chatGPTBuildNumber)
	targetPath := strings.Split(path, "?")[0]
	req.Header.Set("X-OpenAI-Target-Path", targetPath)
	req.Header.Set("X-OpenAI-Target-Route", targetPath)
}

func parseModels(value any) []ports.ModelDescriptor {
	if object, ok := value.(map[string]any); ok {
		if models, ok := object["models"]; ok {
			return parseModels(models)
		}
		if data, ok := object["data"]; ok {
			return parseModels(data)
		}
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]ports.ModelDescriptor, 0)
	for _, raw := range items {
		object, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := object["slug"].(string)
		if id == "" {
			id, _ = object["id"].(string)
		}
		if id == "" {
			continue
		}
		name := id
		if value, ok := object["title"].(string); ok && value != "" {
			name = value
		}
		result = append(result, ports.ModelDescriptor{UpstreamID: id, DisplayName: name, Capabilities: []string{"chat"}})
	}
	return result
}

func parseSSE(body []byte, emit func(ports.StreamChunk) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var id string
	created := time.Now().Unix()
	var snapshots string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		if raw == "v1" {
			continue
		}
		var generic any
		if json.Unmarshal([]byte(raw), &generic) != nil {
			continue
		}
		if text, ok := generic.(string); ok {
			if text != "" {
				snapshots += text
				if err := emit(ports.StreamChunk{ID: id, Text: text, CreatedAt: created}); err != nil {
					return err
				}
			}
			continue
		}
		event, ok := generic.(map[string]any)
		if !ok {
			continue
		}
		if value := visibleAssistantMessage(event); value != nil {
			if valueID, ok := value["id"].(string); ok && valueID != "" {
				id = valueID
			}
			if next := messageText(value); next != "" && len(next) >= len(snapshots) {
				delta := next[len(snapshots):]
				snapshots = next
				if delta != "" {
					if err := emit(ports.StreamChunk{ID: id, Text: delta, CreatedAt: created}); err != nil {
						return err
					}
				}
			}
		}
		if next, ok := applyWebTextPatch(event, snapshots); ok {
			delta := next
			if len(next) >= len(snapshots) {
				delta = next[len(snapshots):]
			}
			snapshots = next
			if delta != "" {
				if err := emit(ports.StreamChunk{ID: id, Text: delta, CreatedAt: created}); err != nil {
					return err
				}
			}
		}
		if delta, ok := event["delta"].(string); ok && delta != "" {
			if err := emit(ports.StreamChunk{ID: id, Text: delta, CreatedAt: created}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func visibleAssistantMessage(event map[string]any) map[string]any {
	candidates := []any{event}
	if nested, ok := event["v"].(map[string]any); ok {
		candidates = append(candidates, nested)
	}
	for _, raw := range candidates {
		message, ok := raw.(map[string]any)["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(map[string]any)
		role, _ := author["role"].(string)
		if strings.ToLower(role) != "assistant" {
			continue
		}
		metadata, _ := message["metadata"].(map[string]any)
		if hidden, _ := metadata["is_visually_hidden_from_conversation"].(bool); hidden {
			continue
		}
		if recipient, _ := message["recipient"].(string); recipient != "" && recipient != "all" {
			continue
		}
		if channel, _ := message["channel"].(string); channel != "" && channel != "final" {
			continue
		}
		return message
	}
	return nil
}

func messageText(message map[string]any) string {
	content, _ := message["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var builder strings.Builder
	for _, part := range parts {
		if text, ok := part.(string); ok {
			builder.WriteString(text)
		}
	}
	if builder.Len() == 0 {
		if text, ok := content["text"].(string); ok {
			return text
		}
	}
	return builder.String()
}

func applyWebTextPatch(event map[string]any, current string) (string, bool) {
	if event["p"] == "/message/content/parts/0" {
		return applyWebPatchOperation(event, current), true
	}
	operations, hasValue := event["v"]
	if !hasValue {
		return current, false
	}
	if text, ok := operations.(string); ok && event["p"] == nil && event["o"] == nil {
		return current + text, true
	}
	if operation, ok := event["o"].(string); ok && operation == "patch" {
		if items, ok := operations.([]any); ok {
			result := current
			for _, item := range items {
				if patch, ok := item.(map[string]any); ok {
					result, _ = applyWebTextPatch(patch, result)
				}
			}
			return result, true
		}
	}
	return current, false
}

func applyWebPatchOperation(operation map[string]any, current string) string {
	op, _ := operation["o"].(string)
	value, _ := operation["v"].(string)
	switch op {
	case "append":
		return current + value
	case "replace":
		return value
	default:
		return current
	}
}
