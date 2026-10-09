package doubao

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const QRFlow = "qr-login"

type Provisioner struct {
	Client     ProvisionClient
	Repository ports.ProvisionRepository
	Box        ports.SecretBox
	TTL        time.Duration
	Now        func() time.Time
}

func NewProvisioner(client ProvisionClient, repository ports.ProvisionRepository, box ports.SecretBox) *Provisioner {
	return &Provisioner{Client: client, Repository: repository, Box: box, TTL: 10 * time.Minute, Now: time.Now}
}

func (p *Provisioner) Start(ctx context.Context, flow string, payload map[string]any, key string) (map[string]any, error) {
	if flow != QRFlow {
		return nil, fmt.Errorf("unsupported Doubao flow: %s", flow)
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if cached, ok, err := p.Repository.GetProvisionIdempotency(ctx, "doubao", "start:"+flow, key, p.Box); err != nil {
		return nil, err
	} else if ok {
		return cached.Response, nil
	}
	accountID, _ := payload["account_id"].(string)
	if accountID == "" {
		accountID = "doubao-" + shortID()
	}
	started, err := p.Client.Start(ctx, accountID)
	if err != nil {
		return nil, err
	}
	now := p.Now()
	sessionID := "doubao-" + shortID()
	state := map[string]any{"account_id": accountID, "csrf_token": started.CSRFToken, "qr_token": started.Token, "qr_code": started.QRCode, "qr_image_base64": started.QRImageBase64, "cookies": started.Cookies, "worker_session_id": started.SessionID, "device_id": shortID(), "web_id": shortID(), "fp": "verify_" + shortID()}
	response := map[string]any{"channel": "doubao", "session_id": sessionID, "status": "waiting_scan", "account_id": accountID, "qr_code": started.QRCode, "qr_image_base64": started.QRImageBase64, "expires_at": now.Add(p.TTL).UTC().Format(time.RFC3339)}
	session := ports.ProvisionSession{Channel: "doubao", SessionID: sessionID, Flow: flow, Status: "waiting_scan", IdempotencyKey: key, CreatedAt: float64(now.Unix()), ExpiresAt: float64(now.Add(p.TTL).Unix()), State: state}
	if err := p.Repository.SaveProvisionSession(ctx, session, p.Box); err != nil {
		return nil, err
	}
	if err := p.Repository.PutProvisionIdempotency(ctx, "doubao", "start:"+flow, key, sessionID, response, session.ExpiresAt, p.Box); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *Provisioner) Poll(ctx context.Context, sessionID string) (map[string]any, error) {
	session, err := p.Repository.LoadProvisionSession(ctx, "doubao", sessionID, p.Box)
	if err != nil {
		return nil, err
	}
	if session.ExpiresAt <= float64(p.Now().Unix()) {
		session.Status = "expired"
		_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
		return map[string]any{"channel": "doubao", "session_id": sessionID, "status": "expired"}, nil
	}
	if session.Status == "succeeded" || session.Status == "cancelled" || session.Status == "expired" {
		return map[string]any{"channel": "doubao", "session_id": sessionID, "status": session.Status}, nil
	}
	result, err := p.Client.Poll(ctx, session.State)
	if err != nil {
		return nil, err
	}
	session.Status = result.Status
	if result.QRCode != "" {
		session.State["qr_code"] = result.QRCode
	}
	if result.QRImageBase64 != "" {
		session.State["qr_image_base64"] = result.QRImageBase64
	}
	if result.Status == "succeeded" {
		session.State["credentials"] = result.Credentials
	}
	if err := p.Repository.SaveProvisionSession(ctx, session, p.Box); err != nil {
		return nil, err
	}
	response := map[string]any{"channel": "doubao", "session_id": sessionID, "status": result.Status, "account_id": session.State["account_id"], "qr_code": session.State["qr_code"], "qr_image_base64": session.State["qr_image_base64"]}
	if result.Status == "succeeded" {
		response["next_step"] = "complete"
	}
	return response, nil
}

func (p *Provisioner) Complete(ctx context.Context, sessionID, key string) (map[string]any, error) {
	if key == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if cached, ok, err := p.Repository.GetProvisionIdempotency(ctx, "doubao", "complete", key, p.Box); err != nil {
		return nil, err
	} else if ok {
		return cached.Response, nil
	}
	session, err := p.Repository.LoadProvisionSession(ctx, "doubao", sessionID, p.Box)
	if err != nil {
		return nil, err
	}
	if session.Status != "succeeded" {
		return nil, fmt.Errorf("Doubao QR session is not ready to complete")
	}
	credentials, ok := session.State["credentials"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Doubao QR session has no credentials")
	}
	converted := map[string]string{}
	for key, value := range credentials {
		if text, ok := value.(string); ok {
			converted[key] = text
		}
	}
	accountID, _ := session.State["account_id"].(string)
	account := ports.AccountImport{Channel: "doubao", ID: "doubao:" + accountID, NativeID: accountID, Name: accountID, Kind: "cookie", Credentials: converted}
	result, err := p.Repository.ImportAccounts(ctx, []ports.AccountImport{account}, p.Box, ports.AuditEvent{Action: "complete_provision"})
	if err != nil {
		return nil, err
	}
	session.Status = "succeeded"
	session.State = map[string]any{}
	_ = p.Repository.SaveProvisionSession(ctx, session, p.Box)
	response := map[string]any{"channel": "doubao", "session_id": sessionID, "status": "succeeded", "added": result.Added, "accounts": result.Accounts}
	if err := p.Repository.PutProvisionIdempotency(ctx, "doubao", "complete", key, sessionID, response, session.ExpiresAt, p.Box); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *Provisioner) Cancel(ctx context.Context, sessionID string) error {
	session, err := p.Repository.LoadProvisionSession(ctx, "doubao", sessionID, p.Box)
	if err != nil {
		return err
	}
	session.Status = "cancelled"
	if canceller, ok := p.Client.(interface {
		Cancel(context.Context, map[string]any) error
	}); ok {
		_ = canceller.Cancel(ctx, session.State)
	}
	session.State = map[string]any{}
	return p.Repository.SaveProvisionSession(ctx, session, p.Box)
}
func shortID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
