package ports

import "context"

type LogQuery struct {
	Page      int
	PageSize  int
	RequestID string
	Channel   string
	Model     string
	Status    *int
	ErrorKind string
	Stream    *bool
	From      *int64
	To        *int64
}

type RequestLog struct {
	ID               int64   `json:"id"`
	Timestamp        int64   `json:"ts"`
	RequestID        string  `json:"request_id"`
	Channel          string  `json:"channel"`
	KeyID            *int64  `json:"key_id"`
	Model            string  `json:"model"`
	UpstreamModel    *string `json:"upstream_model,omitempty"`
	RouteAlias       *string `json:"route_alias,omitempty"`
	FallbackDepth    int     `json:"fallback_depth"`
	Status           int     `json:"status"`
	ErrorKind        string  `json:"error_kind,omitempty"`
	Stream           bool    `json:"stream"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	UsageReported    bool    `json:"usage_reported"`
	UsageKind        string  `json:"usage_kind"`
	TTFTMS           *int64  `json:"ttft_ms,omitempty"`
	LatencyMS        int64   `json:"latency_ms"`
}

type UsageRow struct {
	Day                    string `json:"day,omitempty"`
	Channel                string `json:"channel,omitempty"`
	Model                  string `json:"model,omitempty"`
	KeyID                  *int64 `json:"key_id,omitempty"`
	Requests               int64  `json:"requests"`
	PromptTokens           int64  `json:"prompt_tokens"`
	CompletionTokens       int64  `json:"completion_tokens"`
	UsageReportedRequests  int64  `json:"usage_reported_requests"`
	UsageEstimatedRequests int64  `json:"usage_estimated_requests"`
}

type UsageSummary struct {
	Requests               int64  `json:"requests"`
	PromptTokens           int64  `json:"prompt_tokens"`
	CompletionTokens       int64  `json:"completion_tokens"`
	UsageReportedRequests  int64  `json:"usage_reported_requests"`
	UsageEstimatedRequests int64  `json:"usage_estimated_requests"`
	From                   string `json:"from"`
	To                     string `json:"to"`
}

type ClearLogsResult struct {
	RequestLogsDeleted int    `json:"request_logs_deleted"`
	UsageDailyDeleted  int    `json:"usage_daily_deleted"`
	LogCutoff          string `json:"log_cutoff"`
	UsageCutoffDay     string `json:"usage_cutoff_day"`
	LogRetentionDays   int    `json:"log_retention_days"`
	UsageRetentionDays int    `json:"usage_retention_days"`
}

type AdminQueryRepository interface {
	ListRequestLogs(context.Context, LogQuery) ([]RequestLog, int, error)
	UsageSummary(context.Context, int) (UsageSummary, error)
	UsageRows(context.Context, int, string) ([]UsageRow, error)
	ClearExpiredLogs(context.Context, int, int, AuditEvent) (ClearLogsResult, error)
}
