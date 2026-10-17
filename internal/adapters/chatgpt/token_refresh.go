package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// tokenRefreshMargin renews OAuth access tokens shortly before they expire.
const tokenRefreshMargin = 5 * time.Minute

// tokenRefresher renews expiring OAuth access tokens with the stored
// refresh_token and persists the result, so imported Codex/OAuth accounts
// keep working past the access token lifetime.
type tokenRefresher struct {
	OAuth   *OAuthClient
	Persist func(ctx context.Context, accountID string, credentials map[string]string) error
	locks   sync.Map
}

// EnableTokenRefresh turns on automatic access token renewal.
func (c *Client) EnableTokenRefresh(oauth *OAuthClient, persist func(context.Context, string, map[string]string) error) {
	c.refresher = &tokenRefresher{OAuth: oauth, Persist: persist}
}

// credentialsFor resolves an account credential and renews it when the
// access token is about to expire and a refresh token is available.
func (c *Client) credentialsFor(ctx context.Context, accountID string) (map[string]string, error) {
	if c.ResolveCredential == nil {
		return nil, fmt.Errorf("ChatGPT credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	refresher := c.refresher
	if refresher == nil || refresher.OAuth == nil || strings.TrimSpace(credentials["refresh_token"]) == "" {
		return credentials, nil
	}
	expiresAt, ok := accessTokenExpiry(credentials)
	if !ok || time.Until(expiresAt) > tokenRefreshMargin {
		return credentials, nil
	}
	lockValue, _ := refresher.locks.LoadOrStore(accountID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	// Another request may have rotated the token while we waited.
	if latest, err := c.ResolveCredential(ctx, accountID); err == nil {
		if expiry, ok := accessTokenExpiry(latest); ok && time.Until(expiry) > tokenRefreshMargin {
			return latest, nil
		}
		credentials = latest
	}
	updated, err := refresher.OAuth.Refresh(ctx, credentials["refresh_token"], credentials["client_id"])
	if err != nil {
		// Keep serving the current token if it is still valid; otherwise the
		// upstream 401 will mark the account for re-login.
		if time.Until(expiresAt) > 0 {
			return credentials, nil
		}
		return nil, &HTTPError{Status: 401, Operation: "oauth token refresh"}
	}
	merged := make(map[string]string, len(credentials)+len(updated))
	for key, value := range credentials {
		merged[key] = value
	}
	for key, value := range updated {
		merged[key] = value
	}
	if expiry, ok := accessTokenExpiry(merged); ok {
		merged["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	if refresher.Persist != nil {
		if err := refresher.Persist(ctx, accountID, merged); err != nil {
			return nil, fmt.Errorf("ChatGPT refreshed token could not be stored: %w", err)
		}
	}
	return merged, nil
}

// accessTokenExpiry reads the JWT exp claim, falling back to expires_at.
func accessTokenExpiry(credentials map[string]string) (time.Time, bool) {
	token := strings.TrimSpace(credentials["access_token"])
	if token == "" {
		token = strings.TrimSpace(credentials["token"])
	}
	if parts := strings.Split(token, "."); len(parts) >= 2 {
		if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "=")); err == nil {
			var claims struct {
				Exp float64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp > 0 {
				return time.Unix(int64(claims.Exp), 0), true
			}
		}
	}
	if value := strings.TrimSpace(credentials["expires_at"]); value != "" {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
