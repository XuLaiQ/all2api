package ports

import "context"

type MediaQuery struct {
	Page     int
	PageSize int
	Actor    string
	Kind     string
	Channel  string
	Search   string
}

type MediaAsset struct {
	ID             string         `json:"id"`
	Actor          string         `json:"actor"`
	RunID          *string        `json:"run_id,omitempty"`
	ConversationID *string        `json:"conversation_id,omitempty"`
	Channel        string         `json:"channel"`
	Model          string         `json:"model"`
	Kind           string         `json:"kind"`
	MIMEType       string         `json:"mime_type"`
	Filename       string         `json:"filename"`
	StoragePath    *string        `json:"storage_path,omitempty"`
	SourceURL      *string        `json:"source_url,omitempty"`
	SizeBytes      int64          `json:"size_bytes"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      int64          `json:"created_at"`
	UpdatedAt      int64          `json:"updated_at"`
}

type MediaRepository interface {
	ListMedia(context.Context, MediaQuery) ([]MediaAsset, int, error)
	GetMedia(context.Context, string, string) (MediaAsset, error)
	DeleteMedia(context.Context, string, string, AuditEvent) error
	AddMediaBytes(context.Context, MediaAsset, []byte) (MediaAsset, error)
	AddMediaRemote(context.Context, MediaAsset, string) (MediaAsset, error)
	MediaContentPath(MediaAsset) (string, bool)
}
