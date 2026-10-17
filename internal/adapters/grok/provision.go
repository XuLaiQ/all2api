package grok

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

// DeviceFlow signs a Grok Build account in with the xAI device authorization
// grant: the admin opens the verification URL, enters the code, and polling
// stores the resulting tokens as a new (or renewed) account.
const DeviceFlow = "device-code"

type Provisioner struct {
	OAuth       *BuildOAuth
	Repository  ports.ProvisionRepository
	Credentials ports.CredentialRepository
	Box         ports.SecretBox
	Now         func() time.Time
}

func NewProvisioner(oauth *BuildOAuth, repository ports.ProvisionRepository, box ports.SecretBox) *Provisioner {
	var credentials ports.CredentialRepository
	if candidate, ok := repository.(ports.CredentialRepository); ok {
		credentials = candidate
	}
	return &Provisioner{OAuth: oauth, Repository: repository, Credentials: credentials, Box: box, Now: time.Now}
}

func (p *Provisioner) Start(ctx context.Context, flow string, _ map[string]any, idempotency string) (map[string]any, error) {
	if flow != DeviceFlow {
		return nil, fmt.Errorf("unsupported Grok flow: %s", flow)
	}
	if strings.TrimSpace(idempotency) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if p.OAuth == nil || p.Repository == nil {
		return nil, fmt.Errorf("Grok device login is unavailable")
	}
	if cached, ok, err := p.Repository.GetProvisionIdempotency(ctx, "grok", "start:"+flow, idempotency, p.Box); err != nil {
		return nil, err
	} else if ok {
		return cached.Response, nil
	}
	device, err := p.OAuth.StartDevice(ctx)
	if err != nil {
		return nil, err
	}
	now := p.Now()
	sessionID := "grok_" + randomHex(18)
	expiresAt := now.Add(device.ExpiresIn)
	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}
	interval := int(device.Interval / time.Second)
	response := map[string]any{
		"channel": "grok", "session_id": sessionID, "flow": flow, "status": "waiting_user",
		"auth_url": authURL, "user_code": device.UserCode, "retry_after": interval, "poll_after_seconds": interval,
		"message":    fmt.Sprintf("在打开的 xAI 页面登录并确认代码 %s，完成后会自动添加账号", device.UserCode),
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	}
	session := ports.ProvisionSession{
		Channel: "grok", SessionID: sessionID, Flow: flow, Status: "waiting_user", IdempotencyKey: idempotency,
		CreatedAt: float64(now.Unix()), ExpiresAt: float64(expiresAt.Unix()),
		State: map[string]any{"device_code": device.DeviceCode, "user_code": device.UserCode, "auth_url": authURL, "interval": interval, "next_poll": float64(now.Unix())},
	}
	if err := p.Repository.SaveProvisionSession(ctx, session, p.Box); err != nil {
		return nil, err
	}
	if err := p.Repository.PutProvisionIdempotency(ctx, "grok", "start:"+flow, idempotency, sessionID, response, session.ExpiresAt, p.Box); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *Provisioner) Poll(ctx context.Context, sessionID string) (map[string]any, error) {
	session, err := p.Repository.LoadProvisionSession(ctx, "grok", sessionID, p.Box)
	if err != nil {
		return nil, err
	}
	base := map[string]any{"channel": "grok", "session_id": sessionID, "flow": session.Flow}
	now := p.Now()
	if session.Status == "succeeded" || session.Status == "cancelled" || session.Status == "expired" || session.Status == "failed" {
		base["status"] = session.Status
		if message, _ := session.State["message"].(string); message != "" {
			base["message"] = message
		}
		return base, nil
	}
	if session.ExpiresAt <= float64(now.Unix()) {
		session.Status = "expired"
		session.State = map[string]any{}
		_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
		base["status"] = "expired"
		return base, nil
	}
	interval := 0
	switch typed := session.State["interval"].(type) {
	case float64:
		interval = int(typed)
	case int:
		interval = typed
	}
	if interval <= 0 {
		interval = 5
	}
	waiting := func() map[string]any {
		base["status"] = "waiting_user"
		base["auth_url"] = session.State["auth_url"]
		base["user_code"] = session.State["user_code"]
		base["retry_after"] = interval
		base["poll_after_seconds"] = interval
		base["message"] = fmt.Sprintf("等待在 xAI 页面确认代码 %v", session.State["user_code"])
		return base
	}
	// Respect the server-mandated interval even if the UI polls faster.
	if next, _ := session.State["next_poll"].(float64); next > float64(now.Unix()) {
		return waiting(), nil
	}
	deviceCode, _ := session.State["device_code"].(string)
	tokens, err := p.OAuth.PollDevice(ctx, deviceCode)
	switch {
	case errors.Is(err, ErrAuthorizationPending), errors.Is(err, ErrSlowDown):
		if errors.Is(err, ErrSlowDown) {
			interval += 5
			session.State["interval"] = interval
		}
		session.State["next_poll"] = float64(now.Add(time.Duration(interval) * time.Second).Unix())
		_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
		return waiting(), nil
	case errors.Is(err, ErrAuthorizationDenied):
		session.Status = "failed"
		session.State = map[string]any{"message": "授权被拒绝或已过期，请重新开始"}
		_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
		base["status"] = "failed"
		base["error"] = "授权被拒绝或已过期，请重新开始"
		return base, nil
	case err != nil:
		return nil, err
	}
	result, err := p.importTokens(ctx, tokens)
	if err != nil {
		return nil, err
	}
	session.Status = "succeeded"
	session.State = map[string]any{}
	if err := p.Repository.SaveProvisionSession(ctx, session, p.Box); err != nil {
		return nil, err
	}
	base["status"] = "succeeded"
	base["next_step"] = "done"
	base["added"] = result.Added
	base["updated"] = result.Updated
	base["accounts"] = result.Accounts
	return base, nil
}

// importTokens stores the device-flow tokens as a Build account keyed by the
// xAI user id, so signing the same user in again renews its credential.
func (p *Provisioner) importTokens(ctx context.Context, tokens BuildTokens) (ports.AccountImportResult, error) {
	claims := jwtClaims(tokens.IDToken)
	if claims == nil {
		claims = jwtClaims(tokens.AccessToken)
	}
	userID, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	identity := strings.TrimSpace(userID)
	if identity == "" {
		identity = strings.ToLower(strings.TrimSpace(email))
	}
	if identity == "" {
		identity = randomHex(12)
	}
	name := strings.TrimSpace(email)
	if name == "" {
		name = "Grok Build " + identity
	}
	credentials := map[string]string{
		"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken,
		"auth_mode": "build", "provider": "grok_build", "client_id": p.OAuth.ClientID,
		"expires_at": tokens.ExpiresAt.UTC().Format(time.RFC3339Nano), "token_type": "Bearer",
	}
	if userID != "" {
		credentials["user_id"] = userID
	}
	if email != "" {
		credentials["email"] = email
	}
	if tokens.IDToken != "" {
		credentials["id_token"] = tokens.IDToken
	}
	nativeID := "build:" + identity
	account := ports.AccountImport{Channel: "grok", ID: "grok:" + nativeID, NativeID: nativeID, Name: name, Kind: "oauth", Credentials: credentials}
	return p.Repository.ImportAccounts(ctx, []ports.AccountImport{account}, p.Box, ports.AuditEvent{Action: "complete_provision"})
}

func (p *Provisioner) Cancel(ctx context.Context, sessionID string) error {
	session, err := p.Repository.LoadProvisionSession(ctx, "grok", sessionID, p.Box)
	if err != nil {
		return err
	}
	session.Status = "cancelled"
	session.State = map[string]any{}
	return p.Repository.SaveProvisionSession(ctx, session, p.Box)
}

// Refresh rotates the Build tokens of one stored account on demand.
func (p *Provisioner) Refresh(ctx context.Context, nativeID, actor, ip string) (map[string]any, error) {
	if p.OAuth == nil || p.Credentials == nil {
		return nil, fmt.Errorf("Grok credential refresh is unavailable")
	}
	credentials, err := p.Credentials.ReadCredential(ctx, "grok", nativeID, p.Box)
	if err != nil {
		return nil, err
	}
	if !isBuildCredential(credentials) {
		return nil, fmt.Errorf("only Grok Build OAuth accounts can be refreshed")
	}
	tokens, err := p.OAuth.Refresh(ctx, credentials["refresh_token"], credentials["client_id"])
	if err != nil {
		return nil, err
	}
	credentials["access_token"] = tokens.AccessToken
	credentials["refresh_token"] = tokens.RefreshToken
	credentials["expires_at"] = tokens.ExpiresAt.UTC().Format(time.RFC3339Nano)
	delete(credentials, "expires_in")
	if tokens.IDToken != "" {
		credentials["id_token"] = tokens.IDToken
	}
	if err := p.Credentials.UpdateCredential(ctx, "grok", nativeID, credentials, p.Box, ports.AuditEvent{Actor: actor, Action: "refresh_credential", IP: ip}); err != nil {
		return nil, err
	}
	return map[string]any{"channel": "grok", "account_id": nativeID, "status": "refreshed", "expires_at": credentials["expires_at"]}, nil
}

func randomHex(size int) string {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
