package workbuddy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type CredentialResolver func(context.Context, string) (map[string]string, error)

type Client struct {
	BaseURL           string
	GlobalBaseURL     string
	HTTPClient        *http.Client
	ResolveCredential CredentialResolver
	UserAgent         string
}

type QRStart struct {
	State   string
	AuthURL string
	Realm   string
}
type QRPoll struct {
	Status       string
	Realm        string
	UID          string
	Name         string
	Domain       string
	EnterpriseID string
	AccessToken  string
	RefreshToken string
	DeviceToken  string
}

func (c *Client) StartQR(ctx context.Context, realm string) (QRStart, error) {
	path := "/v2/plugin/auth/state?platform=CLI"
	body, status, err := c.request(ctx, http.MethodPost, path, []byte("{}"), nil)
	if err != nil {
		return QRStart{}, err
	}
	if status < 200 || status >= 300 {
		return QRStart{}, fmt.Errorf("WorkBuddy QR start returned %d", status)
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			State    string `json:"state"`
			AuthURL  string `json:"authUrl"`
			AuthURL2 string `json:"auth_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code != 0 {
		return QRStart{}, ErrProtocol
	}
	url := envelope.Data.AuthURL
	if url == "" {
		url = envelope.Data.AuthURL2
	}
	if envelope.Data.State == "" || url == "" {
		return QRStart{}, ErrProtocol
	}
	return QRStart{State: envelope.Data.State, AuthURL: url, Realm: realm}, nil
}

func (c *Client) PollQR(ctx context.Context, state, realm string) (QRPoll, error) {
	body, status, err := c.request(ctx, http.MethodGet, "/v2/plugin/auth/token?state="+urlQuery(state), nil, nil)
	if err != nil {
		return QRPoll{}, err
	}
	if status < 200 || status >= 300 {
		return QRPoll{}, fmt.Errorf("WorkBuddy QR token returned %d", status)
	}
	var tokenEnvelope struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &tokenEnvelope); err != nil {
		return QRPoll{}, ErrProtocol
	}
	access := firstString(tokenEnvelope.Data, "accessToken", "access_token")
	if tokenEnvelope.Code != 0 || access == "" {
		return QRPoll{Status: "waiting", Realm: realm}, nil
	}
	body, status, err = c.request(ctx, http.MethodGet, "/v2/plugin/login/account?state="+urlQuery(state), nil, map[string]string{"Authorization": "Bearer " + access})
	if err != nil {
		return QRPoll{}, err
	}
	if status < 200 || status >= 300 {
		return QRPoll{}, fmt.Errorf("WorkBuddy account returned %d", status)
	}
	var accountEnvelope struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &accountEnvelope); err != nil || accountEnvelope.Code != 0 {
		return QRPoll{Status: "waiting", Realm: realm}, nil
	}
	uid := firstString(accountEnvelope.Data, "uid", "userId", "user_id")
	if uid == "" {
		return QRPoll{Status: "waiting", Realm: realm}, nil
	}
	return QRPoll{Status: "ready", Realm: realm, UID: uid, Name: firstString(accountEnvelope.Data, "nickname", "nick", "name"), Domain: firstString(accountEnvelope.Data, "domain"), EnterpriseID: firstString(accountEnvelope.Data, "enterpriseId", "enterprise_id", "tenantId", "tenant_id"), AccessToken: access, RefreshToken: firstString(tokenEnvelope.Data, "refreshToken", "refresh_token"), DeviceToken: firstString(tokenEnvelope.Data, "deviceToken", "device_token")}, nil
}

func NewClient(baseURL string, resolver CredentialResolver) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), GlobalBaseURL: "https://www.workbuddy.ai", HTTPClient: &http.Client{Timeout: 30 * time.Second},
		ResolveCredential: resolver, UserAgent: "CLI/2.63.2 CodeBuddy/2.63.2",
	}
}

func (c *Client) Models(ctx context.Context) ([]ports.ModelDescriptor, error) {
	return c.models(ctx, "", nil)
}

func (c *Client) ModelsWithAccount(ctx context.Context, accountID string) ([]ports.ModelDescriptor, error) {
	if c.ResolveCredential == nil {
		return nil, ErrCredentialUnavailable
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	return c.models(ctx, strings.TrimSpace(credentials["access_token"]), credentials)
}

func (c *Client) Refresh(ctx context.Context, accountID string) (map[string]string, error) {
	if c.ResolveCredential == nil {
		return nil, ErrCredentialUnavailable
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	refresh := strings.TrimSpace(credentials["refresh_token"])
	if refresh == "" {
		return nil, ErrCredentialUnavailable
	}
	token := strings.TrimSpace(credentials["access_token"])
	headers := c.runtimeHeaders(credentials, token)
	headers["X-Refresh-Token"] = refresh
	headers["X-Auth-Refresh-Source"] = "plugin"
	body, status, err := c.requestAt(ctx, c.baseURLFor(credentials), http.MethodPost, "/v2/plugin/auth/token/refresh", []byte("{}"), headers)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, newHTTPError(status, "refresh", body)
	}
	var envelope struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code != 0 {
		return nil, ErrProtocol
	}
	access := firstString(envelope.Data, "accessToken", "access_token")
	if access == "" {
		return nil, ErrProtocol
	}
	result := map[string]string{"access_token": access, "refresh_token": firstString(envelope.Data, "refreshToken", "refresh_token")}
	if result["refresh_token"] == "" {
		result["refresh_token"] = refresh
	}
	for _, key := range []string{"deviceToken", "device_token", "expiresAt", "expires_at", "domain"} {
		if value := firstString(envelope.Data, key); value != "" {
			canonical := key
			if key == "deviceToken" {
				canonical = "device_token"
			}
			if key == "expiresAt" {
				canonical = "expires_at"
			}
			result[canonical] = value
		}
	}
	if result["realm"] == "" {
		result["realm"] = credentials["realm"]
	}
	return result, nil
}

func (c *Client) models(ctx context.Context, token string, credentials map[string]string) ([]ports.ModelDescriptor, error) {
	paths := []string{"/v3/config", "/console/enterprises/personal/models"}
	var lastErr error
	for _, path := range paths {
		extra := c.runtimeHeaders(credentials, token)
		body, status, err := c.requestAt(ctx, c.baseURLFor(credentials), http.MethodGet, path, nil, extra)
		if err != nil {
			lastErr = err
			continue
		}
		if status < 200 || status >= 300 {
			lastErr = newHTTPError(status, "models", body)
			continue
		}
		models := parseModels(body)
		if len(models) > 0 {
			return models, nil
		}
		lastErr = ErrProtocol
	}
	if lastErr == nil {
		lastErr = ErrProtocol
	}
	return nil, lastErr
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
		return ErrCredentialUnavailable
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return ErrCredentialUnavailable
	}
	token := strings.TrimSpace(credentials["access_token"])
	if token == "" {
		return ErrCredentialUnavailable
	}
	messages := append([]ports.ChatMessage(nil), input.Messages...)
	if strings.EqualFold(credentials["realm"], "global") || strings.Contains(strings.ToLower(credentials["domain"]), "workbuddy.ai") {
		if len(messages) > 0 && messages[0].Role != "system" {
			messages = append([]ports.ChatMessage{{Role: "system", Content: "You are a helpful assistant."}}, messages...)
		}
	}
	payload := map[string]any{"model": input.Model, "messages": messages, "stream": true, "stream_options": map[string]any{"include_usage": true}}
	if input.MaxTokens != nil {
		payload["max_tokens"] = *input.MaxTokens
	}
	if input.Temperature != nil {
		payload["temperature"] = *input.Temperature
	}
	if input.TopP != nil {
		payload["top_p"] = *input.TopP
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURLFor(credentials)+"/v2/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	for key, value := range c.runtimeHeaders(credentials, token) {
		request.Header.Set(key, value)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return ports.NewTransportError("stream", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return newHTTPError(response.StatusCode, "stream", body)
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		var payload struct {
			ID      string `json:"id"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &payload) != nil || len(payload.Choices) == 0 {
			return ErrProtocol
		}
		chunk := ports.StreamChunk{ID: payload.ID, Text: payload.Choices[0].Message.Content, FinishReason: payload.Choices[0].FinishReason, CreatedAt: time.Now().Unix()}
		if payload.Usage != nil {
			chunk.Usage = &ports.Usage{PromptTokens: payload.Usage.PromptTokens, CompletionTokens: payload.Usage.CompletionTokens}
		}
		return emit(chunk)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var currentID string
	var created int64
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(raw), &event) != nil {
			continue
		}
		if event.ID != "" {
			currentID = event.ID
		}
		if event.Created != 0 {
			created = event.Created
		}
		chunk := ports.StreamChunk{ID: currentID, CreatedAt: created}
		if len(event.Choices) > 0 {
			chunk.Text = event.Choices[0].Delta.Content
			if event.Choices[0].FinishReason != nil {
				chunk.FinishReason = *event.Choices[0].FinishReason
			}
		}
		if event.Usage != nil {
			chunk.Usage = &ports.Usage{PromptTokens: event.Usage.PromptTokens, CompletionTokens: event.Usage.CompletionTokens}
		}
		if chunk.Text != "" || chunk.FinishReason != "" || chunk.Usage != nil {
			if err := emit(chunk); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, extra map[string]string) ([]byte, int, error) {
	return c.requestAt(ctx, c.BaseURL, method, path, body, extra)
}

func (c *Client) requestAt(ctx context.Context, baseURL, method, path string, body []byte, extra map[string]string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if c.UserAgent != "" {
		request.Header.Set("User-Agent", c.UserAgent)
	}
	for key, value := range extra {
		request.Header.Set(key, value)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, 0, ports.NewTransportError(method+" "+path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	return data, response.StatusCode, nil
}

func newHTTPError(status int, operation string, body []byte) *HTTPError {
	result := &HTTPError{Status: status, Operation: operation}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		result.ProviderCode = providerCode(payload["code"])
		if nested, ok := payload["error"].(map[string]any); ok {
			result.ProviderCode = providerCode(nested["code"])
		}
		if result.ProviderCode == "" {
			result.ProviderCode = providerCode(payload["error_code"])
		}
	}
	return result
}

func providerCode(value any) string {
	switch typed := value.(type) {
	case string:
		return sanitizeProviderCode(typed)
	case float64:
		return sanitizeProviderCode(fmt.Sprintf("%.0f", typed))
	case int:
		return sanitizeProviderCode(fmt.Sprintf("%d", typed))
	}
	return ""
}

func sanitizeProviderCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 64 {
		value = value[:64]
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return "provider_error"
		}
	}
	if value == "" {
		return "provider_error"
	}
	return value
}

func (c *Client) baseURLFor(credentials map[string]string) string {
	if strings.EqualFold(credentials["realm"], "global") || strings.Contains(strings.ToLower(credentials["domain"]), "workbuddy.ai") {
		if c.GlobalBaseURL != "" {
			return c.GlobalBaseURL
		}
	}
	return c.BaseURL
}

func (c *Client) runtimeHeaders(credentials map[string]string, token string) map[string]string {
	if credentials == nil {
		credentials = map[string]string{}
	}
	global := strings.EqualFold(credentials["realm"], "global") || strings.Contains(strings.ToLower(credentials["domain"]), "workbuddy.ai")
	origin := "https://copilot.tencent.com"
	language := "zh-CN"
	if global {
		origin = "https://www.workbuddy.ai"
		language = "en-US"
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Origin": origin, "Referer": origin + "/", "X-Requested-With": "XMLHttpRequest", "X-CodeBuddy-Request": "1", "Accept-Language": language, "Content-Type": "application/json", "User-Agent": c.UserAgent}
	uid := credentials["uid"]
	if uid == "" {
		uid = credentials["user_id"]
	}
	if uid != "" {
		headers["X-User-Id"] = uid
		headers["X-Machine-ID"] = stableID("machine", uid)
		headers["X-Session-ID"] = stableID("session", uid)
	}
	if domain := credentials["domain"]; domain != "" {
		headers["X-Domain"] = domain
	} else if global {
		headers["X-Domain"] = "www.workbuddy.ai"
	}
	enterprise := credentials["enterprise_id"]
	if enterprise == "" {
		enterprise = credentials["tenant_id"]
	}
	if enterprise != "" {
		headers["X-Enterprise-Id"] = enterprise
		headers["X-Tenant-Id"] = enterprise
	} else if global {
		headers["X-No-Enterprise-Id"] = "1"
	}
	if device := credentials["device_token"]; device != "" {
		headers["X-Device-Token"] = device
	}
	return headers
}

func stableID(prefix, value string) string {
	digest := sha256.Sum256([]byte(prefix + ":" + value))
	return hex.EncodeToString(digest[:])[:32]
}

func parseModels(body []byte) []ports.ModelDescriptor {
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	return parseModelValue(payload)
}

func parseModelValue(value any) []ports.ModelDescriptor {
	if object, ok := value.(map[string]any); ok {
		if models, ok := object["models"]; ok {
			return parseModelValue(models)
		}
		if data, ok := object["data"]; ok {
			return parseModelValue(data)
		}
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]ports.ModelDescriptor, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && text != "" {
			result = append(result, ports.ModelDescriptor{UpstreamID: text, DisplayName: text, Capabilities: []string{"chat"}})
			continue
		}
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := object["id"].(string)
		if id == "" {
			id, _ = object["modelId"].(string)
		}
		if id == "" || object["disabled"] == true {
			continue
		}
		result = append(result, ports.ModelDescriptor{UpstreamID: id, DisplayName: stringValue(object["name"], id), Capabilities: []string{"chat"}})
	}
	return result
}

func stringValue(value any, fallback string) string {
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return text
	}
	return fallback
}

func firstString(values map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := values[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func urlQuery(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(value, "%", "%25"), " ", "%20"), "+", "%2B")
}
