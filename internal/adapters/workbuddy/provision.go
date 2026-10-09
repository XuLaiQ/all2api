package workbuddy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const QRFlow = "qr-oauth"

type Provisioner struct {
	Client      *Client
	Repository  ports.ProvisionRepository
	Credentials ports.CredentialRepository
	Box         ports.SecretBox
	TTL         time.Duration
	Now         func() time.Time
}

func NewProvisioner(client *Client, repository ports.ProvisionRepository, box ports.SecretBox) *Provisioner {
	var credentials ports.CredentialRepository
	if candidate, ok := repository.(ports.CredentialRepository); ok {
		credentials = candidate
	}
	return &Provisioner{Client: client, Repository: repository, Credentials: credentials, Box: box, TTL: 5 * time.Minute, Now: time.Now}
}

func (p *Provisioner) Start(ctx context.Context, flow string, payload map[string]any, idempotency string) (map[string]any, error) {
	if flow != QRFlow {
		return nil, fmt.Errorf("unsupported WorkBuddy flow: %s", flow)
	}
	if strings.TrimSpace(idempotency) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if p.Repository == nil || p.Client == nil {
		return nil, fmt.Errorf("provisioner is unavailable")
	}
	if cached, ok, err := p.Repository.GetProvisionIdempotency(ctx, "wb", "start:"+flow, idempotency, p.Box); err != nil {
		return nil, err
	} else if ok {
		return cached.Response, nil
	}
	realm := "cn"
	if value, ok := payload["realm"].(string); ok && value != "" {
		realm = strings.ToLower(value)
	}
	if realm != "cn" && realm != "global" {
		return nil, fmt.Errorf("realm must be cn or global")
	}
	started, err := p.Client.StartQR(ctx, realm)
	if err != nil {
		return nil, err
	}
	now := p.Now()
	sessionID, err := randomID("wb_")
	if err != nil {
		return nil, err
	}
	response := map[string]any{"channel": "wb", "session_id": sessionID, "flow": flow, "status": "waiting_user", "realm": realm, "auth_url": started.AuthURL, "expires_at": now.Add(p.TTL).UTC().Format(time.RFC3339)}
	state := map[string]any{"realm": realm, "provider_state": started.State, "auth_url": started.AuthURL}
	if err := p.Repository.SaveProvisionSession(ctx, ports.ProvisionSession{Channel: "wb", SessionID: sessionID, Flow: flow, Status: "waiting_user", IdempotencyKey: idempotency, CreatedAt: float64(now.Unix()), ExpiresAt: float64(now.Add(p.TTL).Unix()), State: state}, p.Box); err != nil {
		return nil, err
	}
	if err := p.Repository.PutProvisionIdempotency(ctx, "wb", "start:"+flow, idempotency, sessionID, response, float64(now.Add(p.TTL).Unix()), p.Box); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *Provisioner) Poll(ctx context.Context, sessionID string) (map[string]any, error) {
	session, err := p.Repository.LoadProvisionSession(ctx, "wb", sessionID, p.Box)
	if err != nil {
		return nil, err
	}
	if session.ExpiresAt <= float64(p.Now().Unix()) {
		session.Status = "expired"
		_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
		return map[string]any{"channel": "wb", "session_id": sessionID, "status": "expired"}, nil
	}
	if session.Status == "succeeded" || session.Status == "cancelled" || session.Status == "expired" {
		return map[string]any{"channel": "wb", "session_id": sessionID, "status": session.Status}, nil
	}
	providerState, _ := session.State["provider_state"].(string)
	realm, _ := session.State["realm"].(string)
	result, err := p.Client.PollQR(ctx, providerState, realm)
	if err != nil {
		return nil, err
	}
	if result.Status != "ready" {
		session.Status = "waiting_callback"
		_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
		return map[string]any{"channel": "wb", "session_id": sessionID, "status": session.Status, "retry_after": 2}, nil
	}
	session.Status = "validating"
	session.State["ready"] = map[string]any{"uid": result.UID, "nickname": result.Name, "domain": result.Domain, "enterprise_id": result.EnterpriseID, "access_token": result.AccessToken, "refresh_token": result.RefreshToken, "device_token": result.DeviceToken, "realm": result.Realm}
	if err := p.Repository.SaveProvisionSession(ctx, session, p.Box); err != nil {
		return nil, err
	}
	return map[string]any{"channel": "wb", "session_id": sessionID, "status": "ready", "next_step": "complete", "account": map[string]any{"id": "wb:" + result.Realm + ":" + result.UID, "channel": "wb", "native_id": result.Realm + ":" + result.UID, "name": result.Name, "kind": "oauth", "status": "ready", "enabled": true}}, nil
}

func (p *Provisioner) Complete(ctx context.Context, sessionID, idempotency string) (map[string]any, error) {
	if strings.TrimSpace(idempotency) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if cached, ok, err := p.Repository.GetProvisionIdempotency(ctx, "wb", "complete", idempotency, p.Box); err != nil {
		return nil, err
	} else if ok {
		return cached.Response, nil
	}
	session, err := p.Repository.LoadProvisionSession(ctx, "wb", sessionID, p.Box)
	if err != nil {
		return nil, err
	}
	if session.Status != "validating" {
		return nil, fmt.Errorf("WorkBuddy QR session is not ready to complete")
	}
	ready, ok := session.State["ready"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("WorkBuddy session has no account result")
	}
	realm, _ := ready["realm"].(string)
	uid, _ := ready["uid"].(string)
	name, _ := ready["nickname"].(string)
	if name == "" {
		name = uid
	}
	creds := map[string]string{}
	for _, key := range []string{"access_token", "refresh_token", "device_token", "realm", "domain", "enterprise_id"} {
		if value, ok := ready[key].(string); ok && value != "" {
			creds[key] = value
		}
	}
	account := ports.AccountImport{Channel: "wb", ID: "wb:" + realm + ":" + uid, NativeID: realm + ":" + uid, Name: name, Kind: "oauth", Credentials: creds}
	result, err := p.Repository.ImportAccounts(ctx, []ports.AccountImport{account}, p.Box, ports.AuditEvent{Action: "complete_provision"})
	if err != nil {
		return nil, err
	}
	session.Status = "succeeded"
	session.State = map[string]any{}
	_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
	response := map[string]any{"channel": "wb", "session_id": sessionID, "status": "succeeded", "next_step": "done", "added": result.Added, "accounts": result.Accounts}
	if err := p.Repository.PutProvisionIdempotency(ctx, "wb", "complete", idempotency, sessionID, response, session.ExpiresAt, p.Box); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *Provisioner) Cancel(ctx context.Context, sessionID string) error {
	session, err := p.Repository.LoadProvisionSession(ctx, "wb", sessionID, p.Box)
	if err != nil {
		return err
	}
	session.Status = "cancelled"
	session.State = map[string]any{}
	return p.Repository.SaveProvisionSession(ctx, session, p.Box)
}

func (p *Provisioner) Refresh(ctx context.Context, nativeID, actor, ip string) (map[string]any, error) {
	if p.Client == nil || p.Credentials == nil {
		return nil, fmt.Errorf("WorkBuddy credential refresh is unavailable")
	}
	credentials, err := p.Credentials.ReadCredential(ctx, "wb", nativeID, p.Box)
	if err != nil {
		return nil, err
	}
	updated, err := p.Client.Refresh(ctx, nativeID)
	if err != nil {
		return nil, err
	}
	for key, value := range updated {
		credentials[key] = value
	}
	if err := p.Credentials.UpdateCredential(ctx, "wb", nativeID, credentials, p.Box, ports.AuditEvent{Actor: actor, Action: "refresh_credential", IP: ip}); err != nil {
		return nil, err
	}
	return map[string]any{"channel": "wb", "account_id": nativeID, "status": "refreshed"}, nil
}

func randomID(prefix string) (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
