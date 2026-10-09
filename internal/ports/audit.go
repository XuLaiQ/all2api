package ports

import "context"

type AuditQuery struct {
	Page     int
	PageSize int
	Actor    string
	Action   string
	Target   string
}
type AuditRecord struct {
	ID        int64  `json:"id"`
	Timestamp int64  `json:"ts"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
}
type AuditRepository interface {
	ListAudit(context.Context, AuditQuery) ([]AuditRecord, int, error)
}
