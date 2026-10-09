package ports

import (
	"context"
	"time"
)

type PlaygroundConversationSummary struct {
	ID           string `json:"id"`
	Actor        string `json:"actor"`
	Title        string `json:"title"`
	Channel      string `json:"channel"`
	Model        string `json:"model"`
	MessageCount int    `json:"message_count"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

type PlaygroundMessage struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	Content     string `json:"content"`
	Model       string `json:"model,omitempty"`
	RawResponse any    `json:"raw,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

type PlaygroundConversation struct {
	PlaygroundConversationSummary
	Messages []PlaygroundMessage `json:"messages"`
}

type PlaygroundRun struct {
	ID             string  `json:"id"`
	ConversationID *string `json:"conversation_id,omitempty"`
	Actor          string  `json:"actor"`
	Channel        string  `json:"channel"`
	Model          string  `json:"model"`
	Status         string  `json:"status"`
	MessageCount   int     `json:"message_count"`
	RequestBytes   int     `json:"request_bytes"`
	ResponseStatus *int    `json:"response_status,omitempty"`
	ErrorCode      *string `json:"error_code,omitempty"`
	CreatedAt      int64   `json:"created_at"`
	CompletedAt    *int64  `json:"completed_at,omitempty"`
}

type PlaygroundRepository interface {
	ListPlaygroundConversations(context.Context, string, int, int) ([]PlaygroundConversationSummary, int, error)
	GetPlaygroundConversation(context.Context, string, string) (PlaygroundConversation, error)
	DeletePlaygroundConversation(context.Context, string, string, AuditEvent) (int, error)
	ListPlaygroundRuns(context.Context, int, int) ([]PlaygroundRun, int, error)
	CreatePlaygroundConversation(context.Context, PlaygroundConversationSummary) error
	AddPlaygroundMessage(context.Context, string, PlaygroundMessage) error
	CreatePlaygroundRun(context.Context, PlaygroundRun) error
	FinishPlaygroundRun(context.Context, string, string, int, string, time.Time) error
}
