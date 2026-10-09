package chatgpt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
)

func TestOAuthProvisionPersistsEncryptedCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/accounts/oauth/token" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","id_token":"id-1"}`))
	}))
	defer server.Close()

	ctx := context.Background()
	store, err := persistence.Open(ctx, t.TempDir()+"/oauth.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	box := cryptoinfra.NewFernet("oauth-fixture-key")
	provisioner := NewProvisioner(NewOAuthClient(server.URL, ""), store, box)
	started, err := provisioner.Start(ctx, OAuthFlow, map[string]any{"email_hint": "person@example.test"}, "start-1")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	sessionID := started["session_id"].(string)
	parsed, err := url.Parse(started["auth_url"].(string))
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	callback := "http://127.0.0.1:1455/auth/callback?code=code-1&state=" + url.QueryEscape(parsed.Query().Get("state"))
	completed, err := provisioner.Complete(ctx, sessionID, "complete-1", map[string]any{"callback": callback})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if completed["status"] != "succeeded" || completed["added"] != 1 {
		t.Fatalf("unexpected completion: %#v", completed)
	}
	if _, err := store.ReadCredential(ctx, "chatgpt", "oauth:"+accountFingerprint("access-1"), box); err != nil {
		t.Fatalf("stored credential unavailable: %v", err)
	}
	refreshed, err := provisioner.Refresh(ctx, "oauth:"+accountFingerprint("access-1"), "admin", "127.0.0.1")
	if err != nil || refreshed["status"] != "refreshed" {
		t.Fatalf("Refresh() = %#v/%v", refreshed, err)
	}
	reloaded, err := store.LoadProvisionSession(ctx, "chatgpt", sessionID, box)
	if err != nil || reloaded.Status != "succeeded" {
		t.Fatalf("reloaded session = %#v/%v", reloaded, err)
	}
}
