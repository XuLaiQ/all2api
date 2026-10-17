package grok

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type memoryProvisionRepo struct {
	mu       sync.Mutex
	sessions map[string]ports.ProvisionSession
	imported []ports.AccountImport
}

func (m *memoryProvisionRepo) ImportAccounts(_ context.Context, items []ports.AccountImport, _ ports.SecretBox, _ ports.AuditEvent) (ports.AccountImportResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imported = append(m.imported, items...)
	return ports.AccountImportResult{Added: len(items)}, nil
}
func (m *memoryProvisionRepo) SaveProvisionSession(_ context.Context, session ports.ProvisionSession, _ ports.SecretBox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]ports.ProvisionSession{}
	}
	m.sessions[session.SessionID] = session
	return nil
}
func (m *memoryProvisionRepo) LoadProvisionSession(_ context.Context, _ string, id string, _ ports.SecretBox) (ports.ProvisionSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id], nil
}
func (m *memoryProvisionRepo) DeleteProvisionSession(context.Context, string, string) error {
	return nil
}
func (m *memoryProvisionRepo) GetProvisionIdempotency(context.Context, string, string, string, ports.SecretBox) (ports.ProvisionIdempotency, bool, error) {
	return ports.ProvisionIdempotency{}, false, nil
}
func (m *memoryProvisionRepo) PutProvisionIdempotency(context.Context, string, string, string, string, map[string]any, float64, ports.SecretBox) error {
	return nil
}

func TestDeviceFlowWaitsThenImportsBuildAccount(t *testing.T) {
	polls := 0
	idToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","email":"a@example.com"}`)) + ".sig"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device":
			if r.Form.Get("client_id") != buildOAuthClientID || r.Header.Get("x-grok-client-surface") != "ui" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_uri":"https://accounts.x.ai/device","verification_uri_complete":"https://accounts.x.ai/device?code=ABCD-EFGH","interval":1,"expires_in":600}`))
		case "/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","id_token":"` + idToken + `","expires_in":3600}`))
		}
	}))
	defer server.Close()
	oauth := NewBuildOAuth(server.Client(), "1.0.13")
	oauth.DeviceURL, oauth.TokenURL = server.URL+"/device", server.URL+"/token"
	repo := &memoryProvisionRepo{}
	clock := time.Now()
	provisioner := NewProvisioner(oauth, repo, nil)
	provisioner.Now = func() time.Time { return clock }

	started, err := provisioner.Start(context.Background(), DeviceFlow, nil, "idem-1")
	if err != nil || started["user_code"] != "ABCD-EFGH" || started["auth_url"] != "https://accounts.x.ai/device?code=ABCD-EFGH" {
		t.Fatalf("start = %#v/%v", started, err)
	}
	sessionID := started["session_id"].(string)
	waiting, err := provisioner.Poll(context.Background(), sessionID)
	if err != nil || waiting["status"] != "waiting_user" || polls != 1 {
		t.Fatalf("first poll = %#v/%v polls=%d", waiting, err, polls)
	}
	// Polling again before the interval elapses must not hit the token URL.
	if _, err := provisioner.Poll(context.Background(), sessionID); err != nil || polls != 1 {
		t.Fatalf("early poll hit upstream: polls=%d err=%v", polls, err)
	}
	clock = clock.Add(2 * time.Second)
	done, err := provisioner.Poll(context.Background(), sessionID)
	if err != nil || done["status"] != "succeeded" || len(repo.imported) != 1 {
		t.Fatalf("final poll = %#v/%v", done, err)
	}
	account := repo.imported[0]
	if account.NativeID != "build:user-1" || account.Name != "a@example.com" || account.Credentials["refresh_token"] != "refresh-1" || account.Credentials["auth_mode"] != "build" || account.Credentials["user_id"] != "user-1" {
		t.Fatalf("imported account = %#v", account)
	}
}
