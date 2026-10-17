package grok

// xAI OAuth for Grok Build (device authorization + refresh_token rotation).
//
// Ported from grok2api (MIT License, Copyright (c) 2026 Chenyme):
// backend/internal/infra/provider/cli/oauth.go.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	buildOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	buildOAuthScope    = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"
	buildDeviceURL     = "https://auth.x.ai/oauth2/device/code"
	buildTokenURL      = "https://auth.x.ai/oauth2/token"
)

var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrSlowDown             = errors.New("slow down")
	ErrAuthorizationDenied  = errors.New("authorization denied or expired")
)

// OAuthError is a token endpoint failure. Permanent errors (revoked or
// unknown refresh tokens) mean the account must sign in again.
type OAuthError struct {
	Status  int
	Code    string
	Message string
}

func (e *OAuthError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("xAI OAuth %s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("xAI OAuth returned HTTP %d (%s)", e.Status, e.Code)
}

func (e *OAuthError) Permanent() bool {
	switch e.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "access_denied", "expired_token":
		return true
	}
	return false
}

// BuildOAuth talks to auth.x.ai.
type BuildOAuth struct {
	HTTPClient    *http.Client
	ClientID      string
	Scope         string
	DeviceURL     string
	TokenURL      string
	ClientVersion string
}

func NewBuildOAuth(httpClient *http.Client, clientVersion string) *BuildOAuth {
	return &BuildOAuth{HTTPClient: httpClient, ClientID: buildOAuthClientID, Scope: buildOAuthScope, DeviceURL: buildDeviceURL, TokenURL: buildTokenURL, ClientVersion: clientVersion}
}

type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

type BuildTokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
}

func (o *BuildOAuth) StartDevice(ctx context.Context) (DeviceAuthorization, error) {
	form := url.Values{"client_id": {o.ClientID}, "scope": {o.Scope}, "referrer": {"grok-build"}}
	body, status, err := o.post(ctx, o.DeviceURL, form, true)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	if status < 200 || status >= 300 {
		return DeviceAuthorization{}, parseOAuthError(body, status)
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.DeviceCode == "" || payload.UserCode == "" || payload.VerificationURI == "" {
		return DeviceAuthorization{}, errors.New("xAI device authorization response is incomplete")
	}
	if payload.Interval <= 0 {
		payload.Interval = 5
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 1800
	}
	return DeviceAuthorization{
		DeviceCode: payload.DeviceCode, UserCode: payload.UserCode,
		VerificationURI: payload.VerificationURI, VerificationURIComplete: payload.VerificationURIComplete,
		Interval: time.Duration(payload.Interval) * time.Second, ExpiresIn: time.Duration(payload.ExpiresIn) * time.Second,
	}, nil
}

// PollDevice exchanges a device code; it returns ErrAuthorizationPending or
// ErrSlowDown while the user has not finished signing in.
func (o *BuildOAuth) PollDevice(ctx context.Context, deviceCode string) (BuildTokens, error) {
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {o.ClientID}, "device_code": {deviceCode}}
	tokens, err := o.exchange(ctx, form, "", true)
	var oauthErr *OAuthError
	if errors.As(err, &oauthErr) {
		switch oauthErr.Code {
		case "authorization_pending":
			return BuildTokens{}, ErrAuthorizationPending
		case "slow_down":
			return BuildTokens{}, ErrSlowDown
		case "access_denied", "expired_token":
			return BuildTokens{}, ErrAuthorizationDenied
		}
	}
	return tokens, err
}

// Refresh rotates a refresh token. The returned refresh token must replace
// the stored one: xAI invalidates the previous token on rotation.
func (o *BuildOAuth) Refresh(ctx context.Context, refreshToken, clientID string) (BuildTokens, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return BuildTokens{}, &OAuthError{Code: "invalid_grant", Message: "account has no refresh token"}
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = o.ClientID
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {strings.TrimSpace(refreshToken)}}
	return o.exchange(ctx, form, refreshToken, false)
}

func (o *BuildOAuth) exchange(ctx context.Context, form url.Values, fallbackRefresh string, deviceFlow bool) (BuildTokens, error) {
	body, status, err := o.post(ctx, o.TokenURL, form, deviceFlow)
	if err != nil {
		return BuildTokens{}, err
	}
	if status < 200 || status >= 300 {
		return BuildTokens{}, parseOAuthError(body, status)
	}
	var value struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &value); err != nil || value.AccessToken == "" {
		return BuildTokens{}, &OAuthError{Status: status, Code: "missing_access_token", Message: "token response has no access_token"}
	}
	if value.ExpiresIn <= 0 {
		value.ExpiresIn = 3600
	}
	refresh := strings.TrimSpace(value.RefreshToken)
	if refresh == "" {
		refresh = strings.TrimSpace(fallbackRefresh)
	}
	return BuildTokens{AccessToken: value.AccessToken, RefreshToken: refresh, IDToken: value.IDToken, ExpiresAt: time.Now().UTC().Add(time.Duration(value.ExpiresIn) * time.Second)}, nil
}

func (o *BuildOAuth) post(ctx context.Context, endpoint string, form url.Values, deviceFlow bool) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if deviceFlow {
		if o.ClientVersion != "" {
			request.Header.Set("x-grok-client-version", o.ClientVersion)
		}
		request.Header.Set("x-grok-client-surface", "ui")
	}
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("xAI OAuth request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return body, response.StatusCode, err
}

func parseOAuthError(body []byte, status int) error {
	var payload struct {
		Error            any    `json:"error"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	code := ""
	switch typed := payload.Error.(type) {
	case string:
		code = typed
	case map[string]any:
		code, _ = typed["code"].(string)
		if payload.Message == "" {
			payload.Message, _ = typed["message"].(string)
		}
	}
	if code == "" {
		code = fmt.Sprintf("http_%d", status)
	}
	message := payload.ErrorDescription
	if message == "" {
		message = payload.Message
	}
	if len(message) > 200 {
		message = message[:200]
	}
	return &OAuthError{Status: status, Code: code, Message: message}
}

// jwtClaims decodes the payload of a JWT without verifying it; only used to
// read identity hints (sub, email, exp) from tokens we just received.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

// buildTokenExpiry reads expires_at (RFC 3339) or the JWT exp claim.
func buildTokenExpiry(credentials map[string]string) (time.Time, bool) {
	if value := strings.TrimSpace(credentials["expires_at"]); value != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed, true
		}
	}
	if exp, ok := jwtClaims(credentials["access_token"])["exp"].(float64); ok && exp > 0 {
		return time.Unix(int64(exp), 0), true
	}
	return time.Time{}, false
}
