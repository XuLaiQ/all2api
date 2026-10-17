package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/XuLaiQ/all2api/internal/infrastructure/sse"
	"github.com/XuLaiQ/all2api/internal/ports"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	"github.com/google/uuid"
)

var ErrCredentialUnavailable = errors.New("Grok credential is unavailable")

type CredentialResolver func(context.Context, string) (map[string]string, error)

type HTTPError struct {
	Status    int
	Operation string
}

type GatewayError struct {
	Status  int
	Code    string
	Message string
}

func (e *GatewayError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "Grok Gateway returned an error"
}

// PublicMessage exposes account-state errors the operator must act on.
func (e *GatewayError) PublicMessage() string {
	switch e.Code {
	case "refresh_token_invalid", "token_expired":
		return "Grok 账号令牌已失效且无法自动续期，请在账号管理中用“xAI 设备码登录”重新登录该账号"
	case "anti_bot_rejected":
		return "Grok 请求被上游反机器人规则拒绝，请检查代理与账号 Cookie"
	case "usage_limit":
		return "Grok 账号额度已用尽，请稍后再试"
	}
	return ""
}

func (e *GatewayError) StatusCode() int {
	if e.Status > 0 {
		return e.Status
	}
	return http.StatusBadGateway
}

func (e *GatewayError) Kind() string {
	switch e.StatusCode() {
	case http.StatusUnauthorized:
		return "credential_rejected"
	case http.StatusTooManyRequests:
		return "upstream_rate_limited"
	case http.StatusGatewayTimeout:
		return "upstream_timeout"
	default:
		return "upstream_error"
	}
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Grok %s endpoint returned HTTP %d", e.Operation, e.Status)
}

func (e *HTTPError) StatusCode() int { return e.Status }

func (e *HTTPError) Kind() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "credential_rejected"
	case http.StatusTooManyRequests:
		return "upstream_rate_limited"
	case http.StatusGatewayTimeout:
		return "upstream_timeout"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "upstream_unavailable"
	default:
		return "upstream_error"
	}
}

type Client struct {
	BaseURL            string
	BuildBaseURL       string
	HTTPClient         *http.Client
	ResolveCredential  CredentialResolver
	UserAgent          string
	BuildUserAgent     string
	BuildClientVersion string
	BuildClientID      string
	BuildTokenAuth     string
	BuildAgentID       string
	HandshakeTimeout   time.Duration
	ChatTimeout        time.Duration
	// StreamHTTPClient carries Build /responses streams; it has no whole
	// request timeout (silence is bounded by ChatTimeout instead).
	StreamHTTPClient *http.Client
	// ProxyURL is applied to the gateway websocket dialer; nil uses the
	// process proxy environment.
	ProxyURL *url.URL
	// OAuth refreshes Build access tokens; PersistCredential stores the
	// rotated tokens (xAI invalidates the previous refresh token).
	OAuth             *BuildOAuth
	PersistCredential func(ctx context.Context, accountID string, credentials map[string]string) error
	refreshLocks      sync.Map
}

// buildRefreshMargin refreshes Build tokens shortly before they expire.
const buildRefreshMargin = 2 * time.Minute

// SetProxy routes all Grok traffic (HTTP, websocket, OAuth) through proxy.
func (c *Client) SetProxy(proxy string) error {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return nil
	}
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("Grok proxy URL is invalid")
	}
	c.ProxyURL = parsed
	transport := &http.Transport{Proxy: http.ProxyURL(parsed), ForceAttemptHTTP2: true, TLSHandshakeTimeout: 20 * time.Second, IdleConnTimeout: 90 * time.Second}
	c.HTTPClient = &http.Client{Timeout: 20 * time.Second, Transport: transport}
	c.StreamHTTPClient = &http.Client{Transport: transport}
	if c.OAuth != nil {
		c.OAuth.HTTPClient = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	return nil
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
const defaultBuildUserAgent = "grok-shell/1.0.13 (windows; x86_64)"

func NewClient(baseURL string, resolver CredentialResolver) *Client {
	return NewClientWithBuildBase(baseURL, "https://cli-chat-proxy.grok.com/v1", resolver)
}

func NewClientWithBuildBase(baseURL, buildBaseURL string, resolver CredentialResolver) *Client {
	return &Client{
		BaseURL:            strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		BuildBaseURL:       strings.TrimRight(strings.TrimSpace(buildBaseURL), "/"),
		HTTPClient:         &http.Client{Timeout: 20 * time.Second},
		StreamHTTPClient:   &http.Client{},
		OAuth:              NewBuildOAuth(&http.Client{Timeout: 30 * time.Second}, "1.0.13"),
		ResolveCredential:  resolver,
		UserAgent:          defaultUserAgent,
		BuildUserAgent:     defaultBuildUserAgent,
		BuildClientVersion: "1.0.13",
		BuildClientID:      "grok-shell",
		BuildTokenAuth:     "xai-grok-cli",
		BuildAgentID:       uuid.NewString(),
		HandshakeTimeout:   20 * time.Second,
		ChatTimeout:        120 * time.Second,
	}
}

func (c *Client) Models(context.Context) ([]ports.ModelDescriptor, error) {
	return catalogModels(tierHeavy), nil
}

func (c *Client) ModelsWithAccount(ctx context.Context, accountID string) ([]ports.ModelDescriptor, error) {
	credentials, err := c.credentials(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if isBuildCredential(credentials) {
		models, err := c.buildModels(ctx, credentials)
		if isUnauthorized(err) {
			// The token may have been revoked early; rotate once and retry.
			if credentials, err = c.refreshBuildCredential(ctx, accountID, credentials, true); err != nil {
				return nil, err
			}
			return c.buildModels(ctx, credentials)
		}
		return models, err
	}
	return catalogModels(credentials["tier"]), nil
}

func (c *Client) credentials(ctx context.Context, accountID string) (map[string]string, error) {
	if c.ResolveCredential == nil {
		return nil, ErrCredentialUnavailable
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	if accessToken(credentials) == "" {
		return nil, ErrCredentialUnavailable
	}
	if isBuildCredential(credentials) {
		if expiresAt, ok := buildTokenExpiry(credentials); ok && time.Until(expiresAt) < buildRefreshMargin {
			return c.refreshBuildCredential(ctx, accountID, credentials, false)
		}
	}
	return credentials, nil
}

func isUnauthorized(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusUnauthorized
}

// refreshBuildCredential rotates the Build tokens of one account. Calls are
// serialised per account and the stored credential is re-read under the lock,
// so concurrent requests reuse a rotation instead of burning the refresh
// token twice. force skips the "still valid" shortcut (used after a 401).
func (c *Client) refreshBuildCredential(ctx context.Context, accountID string, current map[string]string, force bool) (map[string]string, error) {
	if c.OAuth == nil {
		return nil, &GatewayError{Status: http.StatusUnauthorized, Code: "token_expired", Message: "Grok Build access token expired and refresh is unavailable"}
	}
	lockValue, _ := c.refreshLocks.LoadOrStore(accountID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	if latest, err := c.ResolveCredential(ctx, accountID); err == nil && accessToken(latest) != "" {
		if latest["access_token"] != current["access_token"] {
			return latest, nil // another request already rotated
		}
		current = latest
	}
	if !force {
		if expiresAt, ok := buildTokenExpiry(current); ok && time.Until(expiresAt) >= buildRefreshMargin {
			return current, nil
		}
	}
	tokens, err := c.OAuth.Refresh(ctx, current["refresh_token"], current["client_id"])
	if err != nil {
		var oauthErr *OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Permanent() {
			return nil, &GatewayError{Status: http.StatusUnauthorized, Code: "refresh_token_invalid", Message: "Grok Build refresh token is invalid or revoked; sign the account in again"}
		}
		return nil, ports.NewTransportError("Grok Build token refresh", err)
	}
	updated := make(map[string]string, len(current)+2)
	for key, value := range current {
		updated[key] = value
	}
	updated["access_token"] = tokens.AccessToken
	updated["refresh_token"] = tokens.RefreshToken
	updated["expires_at"] = tokens.ExpiresAt.UTC().Format(time.RFC3339Nano)
	delete(updated, "expires_in")
	if tokens.IDToken != "" {
		updated["id_token"] = tokens.IDToken
	}
	if c.PersistCredential != nil {
		if err := c.PersistCredential(ctx, accountID, updated); err != nil {
			return nil, fmt.Errorf("Grok Build refreshed token could not be stored: %w", err)
		}
	}
	return updated, nil
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

func (c *Client) ChatStream(ctx context.Context, input ports.ChatInput, accountID string, emit func(ports.StreamChunk) error) error {
	credentials, err := c.credentials(ctx, accountID)
	if err != nil {
		return err
	}
	if isBuildCredential(credentials) {
		err := c.buildChatStream(ctx, input, credentials, emit)
		if isUnauthorized(err) { // rejected before any output: rotate once
			if credentials, err = c.refreshBuildCredential(ctx, accountID, credentials, true); err != nil {
				return err
			}
			return c.buildChatStream(ctx, input, credentials, emit)
		}
		return err
	}
	spec, ok := resolveModel(input.Model)
	if !ok {
		return fmt.Errorf("unsupported Grok model: %s", input.Model)
	}
	prompt := formatPrompt(input.Messages)
	if prompt == "" {
		return fmt.Errorf("Grok messages are empty")
	}
	userID, err := c.userID(ctx, credentials)
	if err != nil {
		return err
	}
	endpoint, origin, err := gatewayEndpoint(c.BaseURL, userID)
	if err != nil {
		return err
	}
	// ChatTimeout bounds silence on the socket (reset per frame), not the whole
	// answer, so long reasoning responses that keep streaming are not cut off.
	requestCtx := ctx

	headers := gatewayHeaders(origin, userID, credentials, c.webUserAgent(credentials))
	dialer := *websocket.DefaultDialer
	if c.ProxyURL != nil {
		dialer.Proxy = fhttp.ProxyURL(c.ProxyURL)
	}
	if c.HandshakeTimeout > 0 {
		dialer.HandshakeTimeout = c.HandshakeTimeout
	}
	connection, response, err := dialer.DialContext(requestCtx, endpoint, headers)
	if err != nil {
		if response != nil {
			status := response.StatusCode
			_ = response.Body.Close()
			return &HTTPError{Status: status, Operation: "websocket handshake"}
		}
		return ports.NewTransportError("Grok websocket handshake", err)
	}
	defer connection.Close()
	connection.SetReadLimit(gatewayMaxFrameBytes)

	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-requestCtx.Done():
			_ = connection.Close()
		case <-closed:
		}
	}()

	sender := &gatewaySender{connection: connection}
	responseID := "grok_" + uuid.NewString()
	initialEventID := "evt_init_" + uuid.NewString()
	if err := sender.write(map[string]any{
		"event": map[string]any{
			"type": "session.create", "event_id": initialEventID,
			"session": gatewaySession(spec.Mode),
		},
	}); err != nil {
		return ports.NewTransportError("Grok session.create", err)
	}

	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	go gatewayHeartbeat(sender, stopHeartbeat)

	return readGatewayStream(requestCtx, sender, prompt, responseID, initialEventID, c.ChatTimeout, emit)
}

func isBuildCredential(credentials map[string]string) bool {
	mode := strings.ToLower(strings.TrimSpace(credentials["auth_mode"]))
	provider := strings.ToLower(strings.TrimSpace(credentials["provider"]))
	return mode == "build" || provider == "grok_build" || provider == "grok-build"
}

func (c *Client) buildModels(ctx context.Context, credentials map[string]string) ([]ports.ModelDescriptor, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BuildBaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("Grok Build base URL is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	c.applyBuildHeaders(request, credentials, false)
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, ports.NewTransportError("Grok Build models", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPError{Status: response.StatusCode, Operation: "Build models"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			ModelID string `json:"modelId"`
			Hidden  bool   `json:"hidden"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("Grok Build model response is invalid")
	}
	result := make([]ports.ModelDescriptor, 0, len(payload.Data))
	seen := map[string]bool{}
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = strings.TrimSpace(item.Model)
		}
		if id == "" {
			id = strings.TrimSpace(item.ModelID)
		}
		if id == "" || item.Hidden || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, ports.ModelDescriptor{UpstreamID: id, DisplayName: id, Capabilities: []string{"chat"}})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("Grok Build returned no models")
	}
	return result, nil
}

func (c *Client) buildChatStream(ctx context.Context, input ports.ChatInput, credentials map[string]string, emit func(ports.StreamChunk) error) error {
	prompt := formatPrompt(input.Messages)
	if prompt == "" {
		return fmt.Errorf("Grok Build messages are empty")
	}
	model := strings.TrimSpace(input.Model)
	payload := map[string]any{
		"model":   model,
		"input":   prompt,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}
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
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, strings.TrimRight(c.BuildBaseURL, "/")+"/responses", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	c.applyBuildHeaders(request, credentials, true)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	client := c.StreamHTTPClient
	if client == nil {
		client = &http.Client{}
	}
	// Header wait and stream silence are bounded separately so long answers
	// that keep streaming are never cut off by a whole-request timeout.
	idle := c.ChatTimeout
	if idle <= 0 {
		idle = 120 * time.Second
	}
	headerTimer := time.AfterFunc(idle, cancel)
	response, err := client.Do(request)
	headerTimer.Stop()
	if err != nil {
		if ctx.Err() == nil && requestCtx.Err() != nil {
			err = fmt.Errorf("no response headers within %s", idle)
		}
		return ports.NewTransportError("Grok Build chat", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{Status: response.StatusCode, Operation: "Build chat"}
	}
	reader := sse.NewIdleReader(response.Body, idle, cancel)
	defer reader.Stop()
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		body, readErr := io.ReadAll(io.LimitReader(reader, 8<<20))
		if readErr != nil {
			return ports.NewTransportError("Grok Build chat", readErr)
		}
		return parseBuildResponseJSON(body, emit)
	}
	if err := parseBuildResponsesSSE(reader, emit); err != nil {
		var gatewayErr *GatewayError
		if errors.As(err, &gatewayErr) || ctx.Err() != nil {
			return err
		}
		if requestCtx.Err() != nil {
			return ports.NewTransportError("Grok Build chat", fmt.Errorf("stream idle for %s", idle))
		}
		return err
	}
	return nil
}

func (c *Client) applyBuildHeaders(request *http.Request, credentials map[string]string, trace bool) {
	token := accessToken(credentials)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-XAI-Token-Auth", c.BuildTokenAuth)
	request.Header.Set("x-grok-client-version", c.BuildClientVersion)
	request.Header.Set("x-grok-client-identifier", c.BuildClientID)
	request.Header.Set("x-grok-client-mode", "headless")
	request.Header.Set("User-Agent", firstCredentialValue(credentials, "user_agent", "User-Agent", "user-agent", c.BuildUserAgent))
	if userID := firstCredentialValue(credentials, "user_id", "userId", "uid"); userID != "" {
		if trace {
			request.Header.Set("x-grok-user-id", userID)
		} else {
			request.Header.Set("x-userid", userID)
		}
	}
	if email := strings.TrimSpace(credentials["email"]); email != "" && !trace {
		request.Header.Set("x-email", email)
	}
	if trace {
		request.Header.Set("x-authenticateresponse", "authenticate-response")
		request.Header.Set("x-grok-agent-id", c.BuildAgentID)
		request.Header.Set("x-grok-req-id", uuid.NewString())
		traceID := strings.ReplaceAll(uuid.NewString(), "-", "")
		spanID := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
		request.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func firstCredentialValue(credentials map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(credentials[name]); value != "" {
			return value
		}
	}
	return ""
}

func parseBuildResponseJSON(body []byte, emit func(ports.StreamChunk) error) error {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("Grok Build response is invalid")
	}
	if payload["error"] != nil {
		return fmt.Errorf("Grok Build response contains an error")
	}
	chunk := ports.StreamChunk{ID: stringValue(payload["id"]), CreatedAt: time.Now().Unix(), Text: buildResponseText(payload), FinishReason: "stop"}
	if usage, ok := payload["usage"].(map[string]any); ok {
		chunk.Usage = &ports.Usage{PromptTokens: intNumber(usage["input_tokens"]), CompletionTokens: intNumber(usage["output_tokens"])}
	}
	return emit(chunk)
}

func buildResponseText(value map[string]any) string {
	output, _ := value["output"].([]any)
	var builder strings.Builder
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		content, _ := item["content"].([]any)
		for _, rawContent := range content {
			part, _ := rawContent.(map[string]any)
			if text := stringValue(part["text"]); text != "" {
				builder.WriteString(text)
			}
		}
	}
	if builder.Len() > 0 {
		return builder.String()
	}
	return stringValue(value["text"])
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func intNumber(value any) int {
	number, _ := value.(float64)
	return int(number)
}

type gatewaySender struct {
	mu         sync.Mutex
	connection *websocket.Conn
}

func (s *gatewaySender) write(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.connection.SetWriteDeadline(time.Now().Add(gatewayWriteTimeout))
	return s.connection.WriteJSON(value)
}

func gatewayHeartbeat(sender *gatewaySender, done <-chan struct{}) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			if sender.write(map[string]any{"event": map[string]any{"type": "ping", "event_id": fmt.Sprintf("evt_hb_%d", now.UnixMilli())}}) != nil {
				// A dead socket must also unblock the reader.
				_ = sender.connection.Close()
				return
			}
		}
	}
}

func gatewaySession(mode string) map[string]any {
	return map[string]any{
		"model": mode,
		"x_grok": map[string]any{
			"protocol_capabilities":   []string{"conversation_attached", "custom_methods_v1"},
			"use_chunk":               true,
			"enable_side_by_side":     true,
			"force_side_by_side":      false,
			"enable_image_generation": true,
			"image_generation_count":  2,
			"disable_text_follow_ups": false,
			"disable_artifact":        true,
			"force_concise":           false,
			"keep_context":            false,
			"is_temporary":            true,
			"disable_memory":          true,
		},
	}
}

func gatewayTurnEvents(sessionID, prompt string) (map[string]any, map[string]any) {
	now := time.Now().UnixMilli()
	item := map[string]any{
		"session_id": sessionID,
		"event": map[string]any{
			"type": "conversation.item.create", "event_id": fmt.Sprintf("evt_msg_%d", now),
			"item": map[string]any{
				"type": "message", "role": "user",
				"x_grok": map[string]any{"client_message_id": uuid.NewString(), "input_chunks": []any{map[string]any{"text": map[string]any{"text": prompt}}}},
			},
		},
	}
	response := map[string]any{
		"session_id": sessionID,
		"event":      map[string]any{"type": "response.create", "event_id": fmt.Sprintf("evt_resp_%d", now)},
	}
	return item, response
}

func gatewayError(event map[string]any) error {
	value, _ := event["error"].(map[string]any)
	message := firstString(value, "message", "detail", "code")
	if message == "" {
		message = "Grok Gateway returned an error"
	}
	lower := strings.ToLower(message)
	code := strings.ToLower(firstString(value, "code", "type"))
	status := http.StatusBadGateway
	if strings.Contains(lower, "unauthor") || strings.Contains(lower, "credential") || strings.Contains(code, "auth") {
		status = http.StatusUnauthorized
	} else if strings.Contains(lower, "rate limit") || strings.Contains(lower, "quota") || strings.Contains(code, "rate") {
		status = http.StatusTooManyRequests
	} else if strings.Contains(lower, "forbidden") || strings.Contains(code, "anti_bot") {
		status = http.StatusForbidden
	}
	return &GatewayError{Status: status, Code: code, Message: message}
}

func (c *Client) userID(ctx context.Context, credentials map[string]string) (string, error) {
	for _, key := range []string{"user_id", "userId", "uid", "sub"} {
		if value := normalizeUserID(credentials[key]); value != "" {
			return value, nil
		}
	}
	token := accessToken(credentials)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/api/auth/session", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cookie", ssoCookie(token, credentials["cloudflare_cookies"]))
	request.Header.Set("Origin", strings.TrimRight(c.BaseURL, "/"))
	request.Header.Set("Referer", strings.TrimRight(c.BaseURL, "/")+"/")
	request.Header.Set("User-Agent", c.webUserAgent(credentials))
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return "", ports.NewTransportError("Grok session identity", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &HTTPError{Status: response.StatusCode, Operation: "session identity"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("Grok session response is invalid")
	}
	if value := normalizeUserID(sessionUserID(payload)); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("Grok account has no valid user_id")
}

func gatewayEndpoint(baseURL, userID string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("Grok platform base URL is invalid")
	}
	origin := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", "", fmt.Errorf("Grok platform base URL scheme is invalid")
	}
	parsed.Path = "/ws/mgw/"
	parsed.RawPath = ""
	parsed.RawQuery = url.Values{"uid": []string{userID}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), origin, nil
}

func gatewayHeaders(origin, userID string, credentials map[string]string, userAgent string) fhttp.Header {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = defaultUserAgent
	}
	headers := fhttp.Header{}
	headers.Set("Origin", origin)
	headers.Set("User-Agent", userAgent)
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("Cookie", ssoCookie(accessToken(credentials), credentials["cloudflare_cookies"])+"; x-userid="+userID)
	return headers
}

func ssoCookie(token, cloudflareCookies string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), "sso=") {
		token = strings.TrimSpace(token[len("sso="):])
	}
	if value, _, found := strings.Cut(token, ";"); found {
		token = strings.TrimSpace(value)
	}
	token = strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(token)
	result := "sso=" + token + "; sso-rw=" + token
	if cookies := sanitizeCookies(cloudflareCookies); cookies != "" {
		result += "; " + cookies
	}
	return result
}

func sanitizeCookies(value string) string {
	parts := strings.Split(value, ";")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(part, "\r", ""), "\n", ""))
		name, cookieValue, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") || strings.ContainsAny(cookieValue, "\r\n\x00") {
			continue
		}
		clean = append(clean, strings.TrimSpace(name)+"="+strings.TrimSpace(cookieValue))
	}
	return strings.Join(clean, "; ")
}

func accessToken(credentials map[string]string) string {
	for _, key := range []string{"access_token", "sso_token", "token"} {
		if value := strings.TrimSpace(credentials[key]); value != "" {
			return value
		}
	}
	return ""
}

func normalizeUserID(value string) string {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.String()
}

func formatPrompt(messages []ports.ChatMessage) string {
	var builder strings.Builder
	for _, message := range messages {
		content := strings.TrimSpace(message.Content)
		if content == "" && len(message.ToolCalls) == 0 {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "" {
			role = "user"
		}
		builder.WriteString("[")
		builder.WriteString(role)
		builder.WriteString("]\n")
		builder.WriteString(content)
		builder.WriteString("\n\n")
	}
	return strings.TrimSpace(builder.String())
}

func firstString(values map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := values[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sessionUserID(payload map[string]any) string {
	for _, containerName := range []string{"session", "user"} {
		container, _ := payload[containerName].(map[string]any)
		if value := firstString(container, "userId", "user_id", "id", "sub"); value != "" {
			return value
		}
	}
	return firstString(payload, "userId", "user_id", "sub", "id")
}
