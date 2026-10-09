package ports

import "context"

type ModelDescriptor struct {
	Channel      string
	UpstreamID   string
	DisplayName  string
	Capabilities []string
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type ChatInput struct {
	Model       string
	Messages    []ChatMessage
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	Stop        []string
	Tools       []ToolDefinition
	ToolChoice  any
	Stream      bool
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

type ChatResult struct {
	ID            string
	Text          string
	FinishReason  string
	CreatedAt     int64
	Usage         *Usage
	Channel       string
	UpstreamModel string
	RouteAlias    string
	FallbackDepth int
}

type CapabilityInput struct {
	Capability string
	Kind       string
	Model      string
	Prompt     string
	Ratio      string
	Size       string
	Quality    string
	Duration   *int
	Images     []string
	Count      int
}

type CapabilityResult struct {
	StatusCode int
	Body       any
}

type Adapter interface {
	Channel() string
	Models(context.Context) ([]ModelDescriptor, error)
	Chat(context.Context, ChatInput) (ChatResult, error)
}

type AccountAwareAdapter interface {
	Adapter
	ChatWithAccount(context.Context, ChatInput, string) (ChatResult, error)
}

type AccountModelAdapter interface {
	Adapter
	ModelsWithAccount(context.Context, string) ([]ModelDescriptor, error)
}

type StreamChunk struct {
	ID            string
	Text          string
	FinishReason  string
	CreatedAt     int64
	Usage         *Usage
	Channel       string
	UpstreamModel string
	RouteAlias    string
	FallbackDepth int
}

type AccountStreamAdapter interface {
	AccountAwareAdapter
	ChatStreamWithAccount(context.Context, ChatInput, string, func(StreamChunk) error) error
}

type AccountCapabilityAdapter interface {
	Adapter
	CapabilityWithAccount(context.Context, CapabilityInput, string) (CapabilityResult, error)
}
