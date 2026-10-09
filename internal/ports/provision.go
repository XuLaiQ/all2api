package ports

import "context"

type AccountImport struct {
	Channel     string
	ID          string
	NativeID    string
	Name        string
	Kind        string
	Credentials map[string]string
}

type AccountImportResult struct {
	Added    int             `json:"added"`
	Skipped  int             `json:"skipped"`
	Errors   []string        `json:"errors"`
	Accounts []AccountRecord `json:"accounts"`
}

type ProvisionRepository interface {
	ImportAccounts(context.Context, []AccountImport, SecretBox, AuditEvent) (AccountImportResult, error)
	SaveProvisionSession(context.Context, ProvisionSession, SecretBox) error
	LoadProvisionSession(context.Context, string, string, SecretBox) (ProvisionSession, error)
	DeleteProvisionSession(context.Context, string, string) error
	GetProvisionIdempotency(context.Context, string, string, string, SecretBox) (ProvisionIdempotency, bool, error)
	PutProvisionIdempotency(context.Context, string, string, string, string, map[string]any, float64, SecretBox) error
}

type CredentialRepository interface {
	ReadCredential(context.Context, string, string, SecretBox) (map[string]string, error)
	UpdateCredential(context.Context, string, string, map[string]string, SecretBox, AuditEvent) error
}

type ProvisionSession struct {
	Channel        string
	SessionID      string
	Flow           string
	Status         string
	IdempotencyKey string
	CreatedAt      float64
	ExpiresAt      float64
	State          map[string]any
}

type ProvisionIdempotency struct {
	Response  map[string]any
	SessionID string
	ExpiresAt float64
}
