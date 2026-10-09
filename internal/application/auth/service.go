package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/XuLaiQ/all2api/internal/config"
	"github.com/XuLaiQ/all2api/internal/ports"
)

const SessionCookieName = "a2a_session"

var (
	ErrLoginNotConfigured      = errors.New("admin login is not configured")
	ErrInvalidCredentials      = errors.New("invalid username or password")
	ErrAdminSessionRequired    = errors.New("valid admin session required")
	ErrManagementTokenMissing  = errors.New("management token is not configured")
	ErrManagementTokenInvalid  = errors.New("management token configuration is invalid")
	ErrManagementTokenRequired = errors.New("valid management token required")
)

type RateLimitedError struct {
	WaitSeconds int
}

func (e *RateLimitedError) Error() string { return "too many failed login attempts" }

type User struct {
	Username  string
	Role      string
	SessionID string
	ExpiresAt *time.Time
}

type LoginResult struct {
	Cookie    string
	User      User
	ExpiresAt time.Time
}

type Service struct {
	cfg       config.Config
	sessions  ports.SessionManager
	limiter   ports.LoginLimiter
	passwords ports.PasswordVerifier
	audit     ports.AuditWriter
	now       func() time.Time
}

func NewService(cfg config.Config, sessions ports.SessionManager, limiter ports.LoginLimiter, passwords ports.PasswordVerifier, audit ports.AuditWriter) *Service {
	return &Service{
		cfg:       cfg,
		sessions:  sessions,
		limiter:   limiter,
		passwords: passwords,
		audit:     audit,
		now:       time.Now,
	}
}

func (s *Service) Configured() bool {
	passwordLengthOK := len(s.cfg.AdminPassword) >= 12 || s.cfg.AllowWeakAdminPassword
	return strings.TrimSpace(s.cfg.AdminUsername) != "" && passwordLengthOK && len(s.cfg.SessionSecret) >= 32 && s.cfg.SessionSecret != "dev-only-change-me"
}

func (s *Service) Login(ctx context.Context, username, password, ip string) (LoginResult, error) {
	if !s.Configured() {
		return LoginResult{}, ErrLoginNotConfigured
	}
	if wait := s.limiter.Begin(ip, username, s.now()); wait > 0 {
		return LoginResult{}, &RateLimitedError{WaitSeconds: wait}
	}
	configuredUsername := strings.TrimSpace(s.cfg.AdminUsername)
	usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(configuredUsername)) == 1
	if !usernameOK || !s.passwords.Verify(password) {
		_ = s.recordAudit(ctx, sanitize(username), "login_failed", sanitize(username), ip)
		return LoginResult{}, ErrInvalidCredentials
	}
	s.limiter.Clear(ip, configuredUsername)
	cookie, expiresUnix, err := s.sessions.Create(configuredUsername, "admin")
	if err != nil {
		return LoginResult{}, fmt.Errorf("create admin session: %w", err)
	}
	expiresAt := time.Unix(expiresUnix, 0).UTC()
	if err := s.recordAudit(ctx, configuredUsername, "login", configuredUsername, ip); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		Cookie:    cookie,
		User:      User{Username: configuredUsername, Role: "admin", ExpiresAt: &expiresAt},
		ExpiresAt: expiresAt,
	}, nil
}

func (s *Service) Current(cookie string) (User, bool) {
	sessionID, record, ok := s.sessions.Read(cookie)
	if !ok {
		return User{}, false
	}
	expiresAt := time.Unix(record.ExpiresAt, 0).UTC()
	return User{Username: record.Username, Role: record.Role, SessionID: sessionID, ExpiresAt: &expiresAt}, true
}

func (s *Service) Require(authorization, cookie string) (User, error) {
	if authorization != "" {
		configured := s.cfg.AdminToken
		if configured == "" {
			return User{}, ErrManagementTokenMissing
		}
		if !strings.HasPrefix(configured, "wbt_") || len(configured) < 36 {
			return User{}, ErrManagementTokenInvalid
		}
		scheme, supplied, ok := strings.Cut(authorization, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || !secureEqual(strings.TrimSpace(supplied), configured) {
			return User{}, ErrManagementTokenRequired
		}
		return User{Username: s.cfg.AdminUsername, Role: "admin"}, nil
	}
	if user, ok := s.Current(cookie); ok {
		return user, nil
	}
	return User{}, ErrAdminSessionRequired
}

func (s *Service) Logout(ctx context.Context, cookie, ip string) error {
	if user, ok := s.Current(cookie); ok {
		if err := s.recordAudit(ctx, user.Username, "logout", user.Username, ip); err != nil {
			return err
		}
	}
	s.sessions.Revoke(cookie)
	return nil
}

func (s *Service) RevokeUser(username string) {
	s.sessions.RevokeUser(username)
}

func (s *Service) recordAudit(ctx context.Context, actor, action, target, ip string) error {
	if s.audit == nil {
		return nil
	}
	safeIP := sanitize(ip)
	if len(safeIP) > 64 {
		safeIP = safeIP[:64]
	}
	return s.audit.RecordAudit(ctx, actor, action, target, "", safeIP)
}

func secureEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func sanitize(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if unicode.IsPrint(char) && char != '\u007f' {
			builder.WriteRune(char)
		}
		if builder.Len() >= 128 {
			break
		}
	}
	return strings.TrimSpace(builder.String())
}
