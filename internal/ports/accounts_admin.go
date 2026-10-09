package ports

import "context"

type AccountQuery struct {
	Page     int
	PageSize int
	Channel  string
	Status   string
	Search   string
}

type AccountRecord struct {
	ID             string         `json:"id"`
	Channel        string         `json:"channel"`
	NativeID       string         `json:"native_id,omitempty"`
	Name           string         `json:"name"`
	Kind           string         `json:"kind"`
	Tier           *string        `json:"tier"`
	Status         string         `json:"status"`
	Enabled        bool           `json:"enabled"`
	QuotaUsed      float64        `json:"quota_used"`
	QuotaTotal     float64        `json:"quota_total"`
	QuotaUnit      string         `json:"quota_unit"`
	ExpiresAt      *int64         `json:"expires_at"`
	CooldownUntil  *int64         `json:"cooldown_until"`
	UpdatedAt      int64          `json:"updated_at"`
	GatewayRuntime map[string]any `json:"gateway_runtime"`
}

type AccountDeleteResult struct {
	ID                 string `json:"id"`
	Channel            string `json:"channel"`
	CredentialsDeleted bool   `json:"credentials_deleted"`
}

type AccountDeleteFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

type AccountBatchDeleteResult struct {
	Requested int                    `json:"requested"`
	Deleted   []AccountDeleteResult  `json:"deleted"`
	Failed    []AccountDeleteFailure `json:"failed"`
}

type AccountRepository interface {
	ListAccounts(context.Context, AccountQuery) ([]AccountRecord, int, error)
	SetAccountEnabled(context.Context, string, bool, AuditEvent) (AccountRecord, error)
	DeleteAccount(context.Context, string, AuditEvent) error
	DeleteAccounts(context.Context, []string, AuditEvent) (AccountBatchDeleteResult, error)
}
