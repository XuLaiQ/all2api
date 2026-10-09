package chatgpt

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/google/uuid"
)

type OAuthClient struct {
	AuthorizeEndpoint string
	TokenEndpoint     string
	ClientID          string
	Audience          string
	RedirectURI       string
	PlatformBase      string
	UserAgent         string
	SecChUA           string
	Impersonate       string
	HTTPClient        HTTPDoer
}

type PKCERequest struct {
	State        string
	Verifier     string
	Challenge    string
	Nonce        string
	DeviceID     string
	AuthorizeURL string
}

const (
	chatGPTOAuthClientID  = "app_2SKx67EdpoN0G6j64rFvigXD"
	chatGPTOAuthAudience  = "https://api.openai.com/v1"
	chatGPTOAuthRedirect  = "https://platform.openai.com/auth/callback"
	chatGPTAuth0Client    = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
	chatGPTOAuthUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	chatGPTOAuthSecChUA   = `"Google Chrome";v="145", "Not?A_Brand";v="8", "Chromium";v="145"`
)

func NewOAuthClient(baseURL, proxy string) *OAuthClient {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://auth.openai.com"
	}
	httpClient, err := newChatGPTHTTPClient(base, strings.TrimSpace(proxy), 30*time.Second)
	if err != nil {
		fallback := &http.Client{Timeout: 30 * time.Second}
		if proxyURL, parseErr := url.Parse(strings.TrimSpace(proxy)); parseErr == nil && proxyURL.Scheme != "" && proxyURL.Host != "" {
			fallback.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}
		httpClient = fallback
	}
	return &OAuthClient{
		AuthorizeEndpoint: base + "/api/accounts/authorize",
		TokenEndpoint:     base + "/api/accounts/oauth/token",
		ClientID:          chatGPTOAuthClientID,
		Audience:          chatGPTOAuthAudience,
		RedirectURI:       chatGPTOAuthRedirect,
		PlatformBase:      "https://platform.openai.com",
		UserAgent:         chatGPTOAuthUserAgent,
		SecChUA:           chatGPTOAuthSecChUA,
		Impersonate:       "chrome110",
		HTTPClient:        httpClient,
	}
}

func (c *OAuthClient) Begin(emailHint string) (PKCERequest, error) {
	state, err := randomToken(32)
	if err != nil {
		return PKCERequest{}, err
	}
	verifier, err := randomToken(64)
	if err != nil {
		return PKCERequest{}, err
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	nonce, err := randomToken(32)
	if err != nil {
		return PKCERequest{}, err
	}
	deviceID := uuid.NewString()
	query := url.Values{
		"client_id":             {c.ClientID},
		"audience":              {c.Audience},
		"redirect_uri":          {c.RedirectURI},
		"device_id":             {deviceID},
		"screen_hint":           {"login_or_signup"},
		"max_age":               {"0"},
		"response_type":         {"code"},
		"scope":                 {"openid profile email offline_access"},
		"response_mode":         {"query"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"auth0Client":           {chatGPTAuth0Client},
	}
	if value := strings.TrimSpace(emailHint); value != "" {
		query.Set("login_hint", value)
	}
	return PKCERequest{State: state, Verifier: verifier, Challenge: challenge, Nonce: nonce, DeviceID: deviceID, AuthorizeURL: c.AuthorizeEndpoint + "?" + query.Encode()}, nil
}

func (c *OAuthClient) Exchange(ctx context.Context, code, verifier string) (map[string]string, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(verifier) == "" {
		return nil, errors.New("OAuth callback is missing an authorization code")
	}
	return c.ExchangeWithDeviceID(ctx, code, verifier, "")
}

func (c *OAuthClient) ExchangeWithDeviceID(ctx context.Context, code, verifier, deviceID string) (map[string]string, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		deviceID = uuid.NewString()
	}
	payload := map[string]any{
		"client_id":     c.ClientID,
		"code_verifier": verifier,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  c.RedirectURI,
	}
	result, err := c.tokenRequest(ctx, payload, "oauth token exchange")
	if err != nil {
		return nil, err
	}
	result["client_id"] = c.ClientID
	result["auth_mode"] = "web"
	result["user_agent"] = c.UserAgent
	result["impersonate"] = c.Impersonate
	result["oai_device_id"] = deviceID
	result["oai_session_id"] = uuid.NewString()
	if accountID := chatGPTAccountID(result["id_token"]); accountID != "" {
		result["chatgpt_account_id"] = accountID
	}
	return result, nil
}

func (c *OAuthClient) Refresh(ctx context.Context, refreshToken, clientID string) (map[string]string, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("OAuth refresh token is required")
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = c.ClientID
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("OAuth refresh request could not be created")
	}
	c.setOAuthHeaders(request)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, ports.NewTransportError("oauth token refresh", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, errors.New("OAuth refresh response could not be read")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("oauth token refresh returned HTTP %d", response.StatusCode)
	}
	result, err := decodeTokenResponse(body)
	if err != nil {
		return nil, err
	}
	result["client_id"] = clientID
	if strings.TrimSpace(result["refresh_token"]) == "" {
		result["refresh_token"] = refreshToken
	}
	result["auth_mode"] = "web"
	return result, nil
}

func (c *OAuthClient) tokenRequest(ctx context.Context, payload map[string]any, operation string) (map[string]string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("OAuth token request could not be encoded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, errors.New("OAuth token request could not be created")
	}
	c.setOAuthHeaders(request)
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, ports.NewTransportError(operation, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, errors.New("OAuth token response could not be read")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned HTTP %d", operation, response.StatusCode)
	}
	return decodeTokenResponse(body)
}

func decodeTokenResponse(body []byte) (map[string]string, error) {
	var values map[string]any
	if json.Unmarshal(body, &values) != nil {
		return nil, errors.New("OAuth token response is invalid")
	}
	access := stringValue(values["access_token"])
	if access == "" {
		return nil, errors.New("OAuth token response has no access token")
	}
	result := map[string]string{"access_token": access}
	for _, key := range []string{"refresh_token", "id_token", "token_type"} {
		if value := stringValue(values[key]); value != "" {
			result[key] = value
		}
	}
	return result, nil
}

func chatGPTAccountID(idToken string) string {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return ""
	}
	encoded := parts[1]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(encoded)
	}
	if err != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		return ""
	}
	claims, _ := payload["https://api.openai.com/auth"].(map[string]any)
	return stringValue(claims["chatgpt_account_id"])
}

func (c *OAuthClient) setOAuthHeaders(request *http.Request) {
	origin := c.AuthOrigin()
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("DNT", "1")
	request.Header.Set("Origin", origin)
	request.Header.Set("Priority", "u=1, i")
	request.Header.Set("Referer", strings.TrimRight(c.PlatformBase, "/")+"/")
	request.Header.Set("Sec-Ch-Ua", c.SecChUA)
	request.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	request.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Sec-Gpc", "1")
	request.Header.Set("User-Agent", c.UserAgent)
}

func (c *OAuthClient) AuthOrigin() string {
	parsed, err := url.Parse(c.AuthorizeEndpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://auth.openai.com"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func ParseCallback(value string) (map[string]string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil, errors.New("OAuth callback is invalid")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return map[string]string{"code": raw}, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("OAuth callback is invalid")
	}
	query := parsed.Query()
	result := map[string]string{}
	for _, key := range []string{"code", "state", "error", "error_description"} {
		if item := strings.TrimSpace(query.Get(key)); item != "" {
			result[key] = item
		}
	}
	if result["error"] != "" {
		return nil, errors.New("OAuth authorization was denied")
	}
	return result, nil
}

func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("OAuth state could not be generated")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
