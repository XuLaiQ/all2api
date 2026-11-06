package doubao

import (
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
)

type CredentialResolver func(context.Context, string) (map[string]string, error)
type HTTPError struct {
	Status    int
	Operation string
	// Code is the upstream business error code (e.g. 710022002), when known.
	Code string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("Doubao %s endpoint returned HTTP %d (code %s)", e.Operation, e.Status, e.Code)
	}
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
		result.Reasoning += chunk.Reasoning
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

func credentialLookup(values map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(values[name]); value != "" {
			return value
		}
	}
	return ""
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
	req, err := c.newRequest(ctx, method, path, body, credentials)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, ports.NewTransportError(method+" "+path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, err
}

// newRequest builds a doubao.com request with the web security query and the
// browser-like headers (cookie, csrf, agw-js-conv) shared by every endpoint.
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte, credentials map[string]string) (*http.Request, error) {
	target := c.BaseURL + path
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	query := parsed.Query()
	for key, value := range doubaoQuery(credentials) {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
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
	return req, nil
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

// parseModels returns the account's upstream model keys followed by the
// doubao2api chat aliases (doubao / doubao-think / doubao-expert ...), which
// select need_deep_think on /chat/completion. It is empty when the upstream
// payload holds no models, so callers can still detect invalid responses.
func parseModels(value any) []ports.ModelDescriptor {
	result := make([]ports.ModelDescriptor, 0)
	seen := map[string]bool{}
	collectDoubaoModels(value, "", &result, seen)
	if len(result) == 0 {
		return result
	}
	for _, model := range staticChatModels() {
		if !seen[model.UpstreamID] {
			seen[model.UpstreamID] = true
			result = append(result, model)
		}
	}
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
			id := firstString(typed, "model_item_key", "modelItemKey", "model_id")
			if id != "" && !seen[id] {
				if subscription, ok := typed["subscribe_config"].(map[string]any); !ok || subscription["need_upgrade"] != true {
					name := firstString(typed, "name", "display_name")
					if name == "" {
						name = id
					}
					if !isUncuratedDoubaoModel(id, name) {
						seen[id] = true
						*result = append(*result, ports.ModelDescriptor{UpstreamID: id, DisplayName: name, Capabilities: []string{"chat"}})
					}
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
						id := firstString(object, "model_item_key", "modelItemKey", "model_id")
						if id == "" || seen[id] {
							continue
						}
						if subscription, ok := object["subscribe_config"].(map[string]any); ok && subscription["need_upgrade"] == true {
							continue
						}
						name := firstString(object, "name", "display_name")
						if name == "" {
							name = id
						}
						if isUncuratedDoubaoModel(id, name) {
							continue
						}
						seen[id] = true
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

func isUncuratedDoubaoModel(id, name string) bool {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || len(id) > 2 || name != id {
		return false
	}
	for _, char := range id {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
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

func mustJSON(value any) string { encoded, _ := json.Marshal(value); return string(encoded) }
