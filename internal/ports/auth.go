package ports

import (
	"context"
	"time"
)

type SessionRecord struct {
	Username       string
	Role           string
	SessionVersion int
	ExpiresAt      int64
	LastSeenAt     int64
}

type SessionManager interface {
	Create(username, role string) (cookie string, expiresAt int64, err error)
	Read(cookie string) (sessionID string, record SessionRecord, ok bool)
	Revoke(cookie string)
	RevokeUser(username string)
}

type LoginLimiter interface {
	Begin(ip, username string, now time.Time) int
	Clear(ip, username string)
}

type PasswordVerifier interface {
	Verify(password string) bool
}

type AuditWriter interface {
	RecordAudit(ctx context.Context, actor, action, target, detail, ip string) error
}
