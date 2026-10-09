package auth

import (
	"context"
	"testing"

	"github.com/XuLaiQ/all2api/internal/config"
	"github.com/XuLaiQ/all2api/internal/infrastructure/security"
)

type auditFixture struct {
	actions []string
}

func (a *auditFixture) RecordAudit(_ context.Context, _, action, _, _, _ string) error {
	a.actions = append(a.actions, action)
	return nil
}

func configuredService(t *testing.T, token string) (*Service, *auditFixture) {
	t.Helper()
	cfg := config.Config{
		AdminUsername:          "admin",
		AdminPassword:          "correct horse battery staple",
		SessionSecret:          "0123456789abcdef0123456789abcdef",
		AdminToken:             token,
		AllowWeakAdminPassword: false,
		SessionDays:            1,
		SessionIdleHours:       1,
		LoginMaxFails:          3,
	}
	audit := &auditFixture{}
	service := NewService(
		cfg,
		security.NewSessionManager(cfg.SessionSecret, cfg.SessionDays, cfg.SessionIdleHours),
		security.NewLoginLimiter(cfg.LoginMaxFails),
		security.NewPasswordVerifierWithSalt(cfg.AdminPassword, []byte("fixed-salt-123456")),
		audit,
	)
	return service, audit
}

func TestLoginSessionRequireAndLogout(t *testing.T) {
	service, audit := configuredService(t, "")
	result, err := service.Login(context.Background(), "admin", "correct horse battery staple", "127.0.0.1")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if result.Cookie == "" || result.User.Role != "admin" {
		t.Fatalf("unexpected login result: %#v", result)
	}
	user, err := service.Require("", result.Cookie)
	if err != nil || user.Username != "admin" || user.SessionID == "" {
		t.Fatalf("Require() = %#v, %v", user, err)
	}
	if err := service.Logout(context.Background(), result.Cookie, "127.0.0.1"); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := service.Require("", result.Cookie); err == nil {
		t.Fatal("Require() accepted a logged-out session")
	}
	if got, want := audit.actions, []string{"login", "logout"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("audit actions = %#v, want %#v", got, want)
	}
}

func TestLoginFailuresAndManagementToken(t *testing.T) {
	service, audit := configuredService(t, "wbt_01234567890123456789012345678901")
	if _, err := service.Login(context.Background(), "admin", "wrong", "127.0.0.1"); err != ErrInvalidCredentials {
		t.Fatalf("wrong password error = %v", err)
	}
	if len(audit.actions) != 1 || audit.actions[0] != "login_failed" {
		t.Fatalf("failed login audit = %#v", audit.actions)
	}
	user, err := service.Require("Bearer wbt_01234567890123456789012345678901", "")
	if err != nil || user.Role != "admin" {
		t.Fatalf("management token result = %#v, %v", user, err)
	}
	if _, err := service.Require("Bearer wrong", ""); err != ErrManagementTokenRequired {
		t.Fatalf("invalid management token error = %v", err)
	}
}

func TestLoginRequiresStrongConfiguration(t *testing.T) {
	service, _ := configuredService(t, "")
	service.cfg.SessionSecret = "short"
	if service.Configured() {
		t.Fatal("Configured() accepted a weak session secret")
	}
	if _, err := service.Login(context.Background(), "admin", "correct horse battery staple", "127.0.0.1"); err != ErrLoginNotConfigured {
		t.Fatalf("weak configuration error = %v", err)
	}
}
