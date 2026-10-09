package keys_test

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	keyapp "github.com/XuLaiQ/all2api/internal/application/keys"
	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
	"github.com/XuLaiQ/all2api/internal/infrastructure/security"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestKeyLifecyclePersistsOnlyHashAndEncryptedSecret(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, t.TempDir()+"/keys.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	service := keyapp.NewService(
		store,
		cryptoinfra.NewFernet("fixture-master-key"),
		security.NewKeyMaterial(),
		func() []string { return []string{"wb", "doubao", "chatgpt"} },
	)

	created, rawKey, err := service.Create(ctx, keyapp.CreateInput{
		Name: "team-a", Channels: []string{"wb"}, Models: []string{"wb/model-a"}, LimitRPM: 1, Actor: "admin",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.Name != "team-a" || created.Prefix != rawKey[:14] || !created.HasEncryptedKey {
		t.Fatalf("unexpected created key: %#v", created)
	}
	if _, err := service.Authenticate(ctx, "Bearer "+rawKey, ""); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if _, err := service.Authenticate(ctx, "Bearer "+rawKey, ""); err != ports.ErrKeyRateLimited {
		t.Fatalf("second Authenticate() error = %v, want rate limit", err)
	}
	if _, err := service.Update(ctx, created.ID, keyapp.PatchInput{Channels: ptr([]string{"wb"}), Models: ptr([]string{"chatgpt/model-a"}), Actor: "admin"}); err == nil {
		t.Fatal("Update() accepted a cross-channel scope")
	}
	rotated, replacement, err := service.Rotate(ctx, created.ID, "admin", "127.0.0.1")
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.Prefix != replacement[:14] || replacement == rawKey {
		t.Fatal("Rotate() did not issue a replacement key")
	}
	if _, err := service.Authenticate(ctx, "Bearer "+rawKey, ""); err != ports.ErrInvalidKey {
		t.Fatalf("old key after rotation error = %v", err)
	}
	if _, err := service.Authenticate(ctx, "Bearer "+replacement, ""); err != nil {
		t.Fatalf("replacement key after rotation error = %v", err)
	}
	if err := service.Revoke(ctx, created.ID, "admin", "127.0.0.1"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if _, err := service.Authenticate(ctx, "Bearer "+replacement, ""); err != ports.ErrInvalidKey {
		t.Fatalf("revoked key error = %v", err)
	}
	rows, total, err := service.List(ctx, ports.KeyListFilter{Page: 1, PageSize: 50})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("List() = %#v, %d, %v", rows, total, err)
	}
	if rows[0].Prefix != replacement[:14] || rows[0].Enabled || rows[0].HasEncryptedKey == false {
		t.Fatalf("unexpected listed key: %#v", rows[0])
	}

	var actions []string
	// The repository writes the key mutation and its audit record in one transaction.
	for _, action := range []string{"key.created", "key.rotated", "key.revoked"} {
		var count int
		db, err := sql.Open("sqlite", store.Path())
		if err != nil {
			t.Fatalf("open audit reader: %v", err)
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_logs WHERE action = ?", action).Scan(&count); err != nil {
			db.Close()
			t.Fatalf("audit query %s: %v", action, err)
		}
		db.Close()
		if count != 1 {
			t.Fatalf("audit action %s count = %d", action, count)
		}
		actions = append(actions, action)
	}
	if len(actions) != 3 {
		t.Fatalf("audit actions = %#v", actions)
	}
}

func ptr[T any](value T) *T { return &value }
