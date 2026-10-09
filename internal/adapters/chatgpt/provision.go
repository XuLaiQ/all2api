package chatgpt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const OAuthFlow = "oauth-pkce"

type Provisioner struct {
	OAuth       *OAuthClient
	Repository  ports.ProvisionRepository
	Credentials ports.CredentialRepository
	Box         ports.SecretBox
	TTL         time.Duration
	Now         func() time.Time
}

func NewProvisioner(oauth *OAuthClient, repository ports.ProvisionRepository, box ports.SecretBox) *Provisioner {
	var credentials ports.CredentialRepository
	if candidate, ok := repository.(ports.CredentialRepository); ok {
		credentials = candidate
	}
	return &Provisioner{OAuth: oauth, Repository: repository, Credentials: credentials, Box: box, TTL: 10 * time.Minute, Now: time.Now}
}

func (p *Provisioner) Start(ctx context.Context, flow string, payload map[string]any, idempotency string) (map[string]any, error) {
	if flow != OAuthFlow {
		return nil, fmt.Errorf("unsupported ChatGPT flow: %s", flow)
	}
	if strings.TrimSpace(idempotency) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if p.OAuth == nil || p.Repository == nil {
		return nil, fmt.Errorf("ChatGPT OAuth provisioner is unavailable")
	}
	if cached, ok, err := p.Repository.GetProvisionIdempotency(ctx, "chatgpt", "start:"+flow, idempotency, p.Box); err != nil {
		return nil, err
	} else if ok {
		return cached.Response, nil
	}
	emailHint, _ := payload["email_hint"].(string)
	request, err := p.OAuth.Begin(emailHint)
	if err != nil {
		return nil, err
	}
	now := p.Now()
	sessionID, err := randomProvisionID("chatgpt_")
	if err != nil {
		return nil, err
	}
	expiresAt := now.Add(p.TTL)
	response := map[string]any{"channel": "chatgpt", "session_id": sessionID, "flow": flow, "status": "waiting_user", "auth_url": request.AuthorizeURL, "redirect_uri": p.OAuth.RedirectURI, "expires_at": expiresAt.UTC().Format(time.RFC3339)}
	state := map[string]any{"state": request.State, "verifier": request.Verifier, "challenge": request.Challenge, "nonce": request.Nonce, "device_id": request.DeviceID, "auth_url": request.AuthorizeURL}
	session := ports.ProvisionSession{Channel: "chatgpt", SessionID: sessionID, Flow: flow, Status: "waiting_callback", IdempotencyKey: idempotency, CreatedAt: float64(now.Unix()), ExpiresAt: float64(expiresAt.Unix()), State: state}
	if err := p.Repository.SaveProvisionSession(ctx, session, p.Box); err != nil {
		return nil, err
	}
	if err := p.Repository.PutProvisionIdempotency(ctx, "chatgpt", "start:"+flow, idempotency, sessionID, response, session.ExpiresAt, p.Box); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *Provisioner) Poll(ctx context.Context, sessionID string) (map[string]any, error) {
	session, err := p.Repository.LoadProvisionSession(ctx, "chatgpt", sessionID, p.Box)
	if err != nil {
		return nil, err
	}
	if session.ExpiresAt <= float64(p.Now().Unix()) {
		session.Status = "expired"
		_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
		return map[string]any{"channel": "chatgpt", "session_id": sessionID, "status": "expired"}, nil
	}
	if session.Status == "succeeded" || session.Status == "cancelled" || session.Status == "expired" {
		return map[string]any{"channel": "chatgpt", "session_id": sessionID, "status": session.Status}, nil
	}
	authURL, _ := session.State["auth_url"].(string)
	return map[string]any{"channel": "chatgpt", "session_id": sessionID, "status": "waiting_callback", "auth_url": authURL, "retry_after": 2}, nil
}

func (p *Provisioner) Complete(ctx context.Context, sessionID, idempotency string, payload map[string]any) (map[string]any, error) {
	if strings.TrimSpace(idempotency) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if p.OAuth == nil || p.Repository == nil {
		return nil, fmt.Errorf("ChatGPT OAuth provisioner is unavailable")
	}
	if cached, ok, err := p.Repository.GetProvisionIdempotency(ctx, "chatgpt", "complete", idempotency, p.Box); err != nil {
		return nil, err
	} else if ok {
		return cached.Response, nil
	}
	session, err := p.Repository.LoadProvisionSession(ctx, "chatgpt", sessionID, p.Box)
	if err != nil {
		return nil, err
	}
	if session.ExpiresAt <= float64(p.Now().Unix()) {
		return nil, fmt.Errorf("ChatGPT OAuth session expired")
	}
	if session.Status != "waiting_callback" {
		return nil, fmt.Errorf("ChatGPT OAuth session is not waiting for a callback")
	}
	callback, _ := payload["callback"].(string)
	values := map[string]string{}
	if strings.TrimSpace(callback) != "" {
		values, err = ParseCallback(callback)
		if err != nil {
			return nil, err
		}
	} else {
		for _, key := range []string{"code", "state", "error"} {
			if value, ok := payload[key].(string); ok {
				values[key] = strings.TrimSpace(value)
			}
		}
	}
	if values["error"] != "" {
		return nil, fmt.Errorf("OAuth authorization was denied")
	}
	if values["state"] == "" || values["state"] != stringValue(session.State["state"]) {
		return nil, fmt.Errorf("OAuth state does not match")
	}
	if values["code"] == "" {
		return nil, fmt.Errorf("OAuth callback is missing an authorization code")
	}
	verifier := stringValue(session.State["verifier"])
	credentials, err := p.OAuth.ExchangeWithDeviceID(ctx, values["code"], verifier, stringValue(session.State["device_id"]))
	if err != nil {
		return nil, err
	}
	accountID := accountFingerprint(credentials["access_token"])
	account := ports.AccountImport{Channel: "chatgpt", ID: "chatgpt:" + accountID, NativeID: "oauth:" + accountID, Name: accountID, Kind: "oauth", Credentials: credentials}
	result, err := p.Repository.ImportAccounts(ctx, []ports.AccountImport{account}, p.Box, ports.AuditEvent{Action: "complete_provision"})
	if err != nil {
		return nil, err
	}
	session.Status = "succeeded"
	session.State = map[string]any{}
	if err := p.Repository.SaveProvisionSession(ctx, session, p.Box); err != nil {
		return nil, err
	}
	response := map[string]any{"channel": "chatgpt", "session_id": sessionID, "status": "succeeded", "next_step": "done", "added": result.Added, "accounts": result.Accounts}
	if err := p.Repository.PutProvisionIdempotency(ctx, "chatgpt", "complete", idempotency, sessionID, response, session.ExpiresAt, p.Box); err != nil {
		return nil, err
	}
	return response, nil
}

func accountFingerprint(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])[:24]
}

func (p *Provisioner) Cancel(ctx context.Context, sessionID string) error {
	session, err := p.Repository.LoadProvisionSession(ctx, "chatgpt", sessionID, p.Box)
	if err != nil {
		return err
	}
	session.Status = "cancelled"
	session.State = map[string]any{}
	return p.Repository.SaveProvisionSession(ctx, session, p.Box)
}

func (p *Provisioner) Refresh(ctx context.Context, nativeID, actor, ip string) (map[string]any, error) {
	if p.OAuth == nil || p.Credentials == nil {
		return nil, fmt.Errorf("ChatGPT credential refresh is unavailable")
	}
	credentials, err := p.Credentials.ReadCredential(ctx, "chatgpt", nativeID, p.Box)
	if err != nil {
		return nil, err
	}
	refreshToken := credentials["refresh_token"]
	if refreshToken == "" {
		return nil, fmt.Errorf("ChatGPT credentials have no refresh token")
	}
	updated, err := p.OAuth.Refresh(ctx, refreshToken, credentials["client_id"])
	if err != nil {
		return nil, err
	}
	for key, value := range updated {
		credentials[key] = value
	}
	if err := p.Credentials.UpdateCredential(ctx, "chatgpt", nativeID, credentials, p.Box, ports.AuditEvent{Actor: actor, Action: "refresh_credential", IP: ip}); err != nil {
		return nil, err
	}
	return map[string]any{"channel": "chatgpt", "account_id": nativeID, "status": "refreshed"}, nil
}

func randomProvisionID(prefix string) (string, error) {
	value, err := randomToken(18)
	if err != nil {
		return "", err
	}
	return prefix + value, nil
}
