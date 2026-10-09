package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
)

func TestQRProvisionPersistsSessionAndCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = writer.Write([]byte(`{"code":0,"data":{"state":"state-1","authUrl":"https://login.example/qr"}}`))
		case "/v2/plugin/auth/token":
			_, _ = writer.Write([]byte(`{"code":0,"data":{"accessToken":"access-1","refreshToken":"refresh-1"}}`))
		case "/v2/plugin/login/account":
			_, _ = writer.Write([]byte(`{"code":0,"data":{"uid":"user-1","nickname":"Demo"}}`))
		case "/v2/plugin/auth/token/refresh":
			_, _ = writer.Write([]byte(`{"code":0,"data":{"accessToken":"access-2","refreshToken":"refresh-2"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	store, err := persistence.Open(ctx, t.TempDir()+"/provision.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	box := cryptoinfra.NewFernet("provision-fixture-key")
	provisioner := NewProvisioner(NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "access-1", "refresh_token": "refresh-1", "realm": "cn"}, nil
	}), store, box)

	started, err := provisioner.Start(ctx, QRFlow, map[string]any{"realm": "cn"}, "start-1")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if started["status"] != "waiting_user" || started["auth_url"] != "https://login.example/qr" {
		t.Fatalf("unexpected start: %#v", started)
	}
	if _, err := provisioner.Start(ctx, QRFlow, map[string]any{"realm": "cn"}, "start-1"); err != nil {
		t.Fatalf("idempotent Start() error = %v", err)
	}

	sessionID := started["session_id"].(string)
	ready, err := provisioner.Poll(ctx, sessionID)
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if ready["status"] != "ready" {
		t.Fatalf("unexpected poll: %#v", ready)
	}

	completed, err := provisioner.Complete(ctx, sessionID, "complete-1")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if completed["status"] != "succeeded" || completed["added"] != 1 {
		t.Fatalf("unexpected complete: %#v", completed)
	}
	if _, err := store.ReadCredential(ctx, "wb", "cn:user-1", box); err != nil {
		t.Fatalf("stored credential unavailable: %v", err)
	}
	refreshed, err := provisioner.Refresh(ctx, "cn:user-1", "admin", "127.0.0.1")
	if err != nil || refreshed["status"] != "refreshed" {
		t.Fatalf("Refresh() = %#v/%v", refreshed, err)
	}

	reloaded, err := store.LoadProvisionSession(ctx, "wb", sessionID, box)
	if err != nil {
		t.Fatalf("LoadProvisionSession() error = %v", err)
	}
	if reloaded.Status != "succeeded" {
		t.Fatalf("reloaded status = %q", reloaded.Status)
	}
	if _, ok, err := store.GetProvisionIdempotency(ctx, "wb", "complete", "complete-1", box); err != nil || !ok {
		t.Fatalf("complete idempotency = %v/%v", ok, err)
	}
}
