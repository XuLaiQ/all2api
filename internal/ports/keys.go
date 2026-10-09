package ports

import (
	"context"
	"errors"
	"time"
)

var (
	ErrKeyNotFound    = errors.New("API key not found")
	ErrKeyNameExists  = errors.New("key name already exists")
	ErrBootstrapKey   = errors.New("bootstrap key is managed by configuration")
	ErrInvalidKey     = errors.New("invalid or expired API key")
	ErrKeyRateLimited = errors.New("API key rate limit exceeded")
)

type APIKey struct {
	ID              int64
	Name            string
	Prefix          string
	Enabled         bool
	ExpiresAt       *int64
	Channels        []string
	Models          []string
	LimitRPM        int
	CreatedAt       int64
	LastUsedAt      *int64
	HasEncryptedKey bool
}

type KeyListFilter struct {
	Page     int
	PageSize int
	Search   string
	Enabled  *bool
}

type KeyCreate struct {
	Name      string
	KeyHash   string
	Encrypted string
	Prefix    string
	ExpiresAt *int64
	Channels  []string
	Models    []string
	LimitRPM  int
	CreatedAt int64
	Audit     AuditEvent
}

type KeyPatch struct {
	Name         *string
	Enabled      *bool
	Channels     *[]string
	Models       *[]string
	ExpiresAt    *int64
	ExpiresAtSet bool
	LimitRPM     *int
	Audit        AuditEvent
}

type AuditEvent struct {
	Actor  string
	Action string
	Target string
	Detail string
	IP     string
}

type KeyRepository interface {
	ListKeys(context.Context, KeyListFilter) ([]APIKey, int, error)
	GetKey(context.Context, int64) (APIKey, error)
	CreateKey(context.Context, KeyCreate) (APIKey, error)
	UpdateKey(context.Context, int64, KeyPatch) (APIKey, error)
	RotateKey(context.Context, int64, string, string, string, AuditEvent) (APIKey, error)
	RevokeKey(context.Context, int64, AuditEvent) error
	AuthenticateKey(context.Context, string, time.Time) (APIKey, error)
}

type SecretBox interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

type KeyMaterial interface {
	Hash(string) string
	Issue() (string, error)
	Equal(string, string) bool
}
