package ports

import (
	"context"
	"time"
)

type RequestRecord struct {
	RequestID        string
	KeyID            int64
	Channel          string
	Model            string
	UpstreamModel    string
	RouteAlias       string
	FallbackDepth    int
	TTFTMS           *int64
	Status           int
	ErrorKind        string
	Error            string
	Stream           bool
	PromptTokens     int
	CompletionTokens int
	UsageReported    bool
	UsageKind        string
	StartedAt        time.Time
	LatencyMS        int64
}

type RequestRecorder interface {
	RecordRequest(context.Context, RequestRecord) error
}
