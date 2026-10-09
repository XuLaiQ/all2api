package ports

import "context"

type SystemChannel struct {
	Slug                string `json:"slug"`
	ModelsConfigured    bool   `json:"models_configured"`
	AccountsConfigured  bool   `json:"accounts_configured"`
	ProvisionConfigured bool   `json:"provision_configured"`
}

type SystemInfo struct {
	Service        string          `json:"service"`
	Version        string          `json:"version"`
	GoVersion      string          `json:"go_version"`
	Platform       string          `json:"platform"`
	Implementation string          `json:"implementation"`
	SchemaVersion  int             `json:"schema_version"`
	Database       map[string]any  `json:"database"`
	RuntimeState   map[string]any  `json:"runtime_state"`
	Channels       []SystemChannel `json:"channels"`
}

type StorageHealth struct {
	Status       string         `json:"status"`
	Database     map[string]any `json:"database"`
	RuntimeState map[string]any `json:"runtime_state"`
	Disk         map[string]any `json:"disk"`
}

type ChannelMetric struct {
	Channel      string  `json:"channel"`
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

type SystemMetrics struct {
	From         string           `json:"from"`
	To           string           `json:"to"`
	Requests     int64            `json:"requests"`
	Errors       int64            `json:"errors"`
	ErrorRate    float64          `json:"error_rate"`
	Streams      int64            `json:"streams"`
	AvgLatencyMS float64          `json:"avg_latency_ms"`
	P95LatencyMS int64            `json:"p95_latency_ms"`
	Channels     []ChannelMetric  `json:"channels"`
	Accounts     map[string]int64 `json:"accounts"`
}

type SystemRepository interface {
	SystemInfo(context.Context, string, []SystemChannel) (SystemInfo, error)
	StorageHealth(context.Context, string) (StorageHealth, error)
	Metrics(context.Context, int) (SystemMetrics, error)
}
