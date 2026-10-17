package grok

import (
	"context"
	"fmt"

	"github.com/XuLaiQ/all2api/internal/ports"
)

// Adapter exposes the Grok Web SSO surface through the common gateway ports.
// Grok Web is account-backed, so all data-plane calls are routed with an
// account selected by the shared scheduler.
type Adapter struct {
	client *Client
}

func NewAdapter(client *Client) *Adapter { return &Adapter{client: client} }

func (a *Adapter) Channel() string { return "grok" }

func (a *Adapter) Models(ctx context.Context) ([]ports.ModelDescriptor, error) {
	return a.client.Models(ctx)
}

func (a *Adapter) ModelsWithAccount(ctx context.Context, accountID string) ([]ports.ModelDescriptor, error) {
	return a.client.ModelsWithAccount(ctx, accountID)
}

func (a *Adapter) Chat(ctx context.Context, input ports.ChatInput) (ports.ChatResult, error) {
	return ports.ChatResult{}, fmt.Errorf("Grok requires an account")
}

func (a *Adapter) ChatWithAccount(ctx context.Context, input ports.ChatInput, accountID string) (ports.ChatResult, error) {
	return a.client.Chat(ctx, input, accountID)
}

func (a *Adapter) ChatStreamWithAccount(ctx context.Context, input ports.ChatInput, accountID string, emit func(ports.StreamChunk) error) error {
	return a.client.ChatStream(ctx, input, accountID, emit)
}
