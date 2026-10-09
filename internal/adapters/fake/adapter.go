package fake

import (
	"context"
	"sync"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type Adapter struct {
	Slug      string
	Catalogue []ports.ModelDescriptor
	Reply     string
	mu        sync.Mutex
	ChatCalls int
}

func (a *Adapter) Channel() string { return a.Slug }

func (a *Adapter) Models(context.Context) ([]ports.ModelDescriptor, error) {
	return append([]ports.ModelDescriptor(nil), a.Catalogue...), nil
}

func (a *Adapter) Chat(_ context.Context, input ports.ChatInput) (ports.ChatResult, error) {
	a.mu.Lock()
	a.ChatCalls++
	a.mu.Unlock()
	return ports.ChatResult{
		ID:           "chatcmpl_fake",
		Text:         a.Reply,
		FinishReason: "stop",
		CreatedAt:    time.Now().Unix(),
		Usage:        &ports.Usage{PromptTokens: len(input.Messages), CompletionTokens: 1},
	}, nil
}
