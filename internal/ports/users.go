package ports

import "context"

type UserRecord struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type UserRepository interface {
	ListUsers(context.Context) ([]UserRecord, error)
	CreateUser(context.Context, UserRecord, AuditEvent) (UserRecord, error)
	UpdateUser(context.Context, string, UserRecord, AuditEvent) (UserRecord, error)
	DeleteUser(context.Context, string, AuditEvent) error
}
