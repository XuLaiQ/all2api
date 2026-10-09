package ports

import "context"

type ModelQuery struct {
	Page     int
	PageSize int
	Channel  string
	Kind     string
	Search   string
	Enabled  *bool
}

type ModelRecord struct {
	ID            string   `json:"id"`
	Channel       string   `json:"channel"`
	UpstreamID    string   `json:"upstream_id"`
	DisplayName   string   `json:"display_name"`
	Kind          string   `json:"kind"`
	Capabilities  []string `json:"caps"`
	ContextWindow *int64   `json:"context_window"`
	MaxOutput     *int64   `json:"max_output"`
	Multiplier    float64  `json:"multiplier"`
	Enabled       bool     `json:"enabled"`
}

type RouteTarget struct {
	Position int    `json:"position"`
	Channel  string `json:"channel"`
	Model    string `json:"model"`
	Weight   int    `json:"weight"`
}

type RouteRecord struct {
	Alias     string        `json:"alias"`
	Strategy  string        `json:"strategy"`
	Enabled   bool          `json:"enabled"`
	CreatedAt int64         `json:"created_at"`
	Targets   []RouteTarget `json:"targets"`
}

type RouteInput struct {
	Strategy string
	Enabled  bool
	Targets  []RouteTarget
}

type ChannelOverride struct {
	Slug         string         `json:"slug"`
	Name         string         `json:"name"`
	Adapter      string         `json:"adapter"`
	UpstreamBase string         `json:"upstream_base"`
	AuthKind     string         `json:"auth_kind"`
	Enabled      bool           `json:"enabled"`
	Config       map[string]any `json:"config"`
	CreatedAt    int64          `json:"created_at"`
	UpdatedAt    int64          `json:"updated_at"`
}

type CatalogRepository interface {
	ListModels(context.Context, ModelQuery) ([]ModelRecord, int, []string, error)
	UpdateModelEnabled(context.Context, string, bool, AuditEvent) (ModelRecord, error)
	ListRoutes(context.Context) ([]RouteRecord, error)
	UpsertRoute(context.Context, string, RouteInput, AuditEvent) (RouteRecord, error)
	DeleteRoute(context.Context, string, AuditEvent) error
	GetSettings(context.Context) (map[string]string, error)
	PutSettings(context.Context, map[string]string, AuditEvent) error
}

type RouteSource interface {
	ListRoutes(context.Context) ([]RouteRecord, error)
}

type ModelSyncRepository interface {
	SyncModels(context.Context, string, []ModelDescriptor, AuditEvent) (int, error)
}

type ChannelRepository interface {
	GetChannelOverride(context.Context, string) (ChannelOverride, bool, error)
	UpsertChannelOverride(context.Context, ChannelOverride, AuditEvent) (ChannelOverride, error)
	DeleteChannelOverride(context.Context, string, AuditEvent) error
}

type ChannelStateSource interface {
	ChannelEnabled(context.Context, string) (bool, error)
}
