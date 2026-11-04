package gateway

import (
	"context"
	"testing"

	"github.com/XuLaiQ/all2api/internal/adapters/fake"
	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/XuLaiQ/all2api/internal/scheduler"
)

type channelStateFixture bool

func (f channelStateFixture) ChannelEnabled(context.Context, string) (bool, error) {
	return bool(f), nil
}

type routeSourceFixture struct{}

func (routeSourceFixture) ListRoutes(context.Context) ([]ports.RouteRecord, error) {
	return []ports.RouteRecord{{Alias: "primary", Enabled: true, Strategy: "priority", Targets: []ports.RouteTarget{{Channel: "wb", Model: "model-a"}}}}, nil
}

type retryableAdapter struct {
	*fake.Adapter
}

func (a retryableAdapter) Chat(context.Context, ports.ChatInput) (ports.ChatResult, error) {
	return ports.ChatResult{}, retryableRouteFixtureError{}
}

type retryableRouteFixtureError struct{}

func (retryableRouteFixtureError) Error() string   { return "upstream fixture failure" }
func (retryableRouteFixtureError) StatusCode() int { return 502 }
func (retryableRouteFixtureError) Kind() string    { return "upstream_unavailable" }

type credentialRejectedFixtureError struct{}

func (credentialRejectedFixtureError) Error() string   { return "credential rejected" }
func (credentialRejectedFixtureError) StatusCode() int { return 401 }
func (credentialRejectedFixtureError) Kind() string    { return "credential_rejected" }

type accountRetryAdapter struct {
	attempts []string
}

func (a *accountRetryAdapter) Channel() string { return "chatgpt" }
func (a *accountRetryAdapter) Models(context.Context) ([]ports.ModelDescriptor, error) {
	return []ports.ModelDescriptor{{UpstreamID: "model-chatgpt", Capabilities: []string{"chat"}}}, nil
}
func (a *accountRetryAdapter) Chat(context.Context, ports.ChatInput) (ports.ChatResult, error) {
	return ports.ChatResult{}, credentialRejectedFixtureError{}
}
func (a *accountRetryAdapter) ChatWithAccount(_ context.Context, _ ports.ChatInput, accountID string) (ports.ChatResult, error) {
	a.attempts = append(a.attempts, accountID)
	if accountID == "expired" {
		return ports.ChatResult{}, credentialRejectedFixtureError{}
	}
	return ports.ChatResult{ID: "ok", Text: "recovered"}, nil
}
func (a *accountRetryAdapter) ChatStreamWithAccount(_ context.Context, _ ports.ChatInput, accountID string, emit func(ports.StreamChunk) error) error {
	a.attempts = append(a.attempts, accountID)
	if accountID == "expired" {
		return credentialRejectedFixtureError{}
	}
	return emit(ports.StreamChunk{ID: "ok", Text: "recovered", FinishReason: "stop"})
}

type accountFixture struct{}

func (accountFixture) Candidates(_ context.Context, channel, _ string, _ bool) ([]ports.AccountCandidate, error) {
	return []ports.AccountCandidate{{ID: channel + ":account", NativeID: channel + "-account", Channel: channel}}, nil
}

type twoAccountFixture struct{}

func (twoAccountFixture) Candidates(_ context.Context, channel, _ string, _ bool) ([]ports.AccountCandidate, error) {
	return []ports.AccountCandidate{
		{ID: channel + ":expired", NativeID: "expired", Channel: channel},
		{ID: channel + ":fresh", NativeID: "fresh", Channel: channel},
	}, nil
}

type streamFixtureAdapter struct {
	channel          string
	failBeforeOutput bool
	failAfterOutput  bool
}

func (a streamFixtureAdapter) Channel() string { return a.channel }
func (a streamFixtureAdapter) Models(context.Context) ([]ports.ModelDescriptor, error) {
	return []ports.ModelDescriptor{{UpstreamID: "model-" + a.channel, Capabilities: []string{"chat"}}}, nil
}
func (a streamFixtureAdapter) Chat(context.Context, ports.ChatInput) (ports.ChatResult, error) {
	return ports.ChatResult{Text: "fallback"}, nil
}
func (a streamFixtureAdapter) ChatWithAccount(ctx context.Context, input ports.ChatInput, _ string) (ports.ChatResult, error) {
	return a.Chat(ctx, input)
}
func (a streamFixtureAdapter) ChatStreamWithAccount(_ context.Context, _ ports.ChatInput, _ string, emit func(ports.StreamChunk) error) error {
	if a.failBeforeOutput {
		return retryableRouteFixtureError{}
	}
	if err := emit(ports.StreamChunk{Text: "first"}); err != nil {
		return err
	}
	if a.failAfterOutput {
		return retryableRouteFixtureError{}
	}
	return nil
}

var _ ports.AccountStreamAdapter = streamFixtureAdapter{}

func TestChatRetriesCredentialRejectedAccountBeforeReturningError(t *testing.T) {
	adapter := &accountRetryAdapter{}
	service := NewServiceWithRecorderAndPool(nil, map[string]ports.Adapter{"chatgpt": adapter}, nil, scheduler.NewPool(twoAccountFixture{}, 1, 3))
	result, err := service.AdminChat(context.Background(), ports.ChatInput{
		Model: "chatgpt/model-chatgpt", Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil || result.Text != "recovered" {
		t.Fatalf("credential rejection retry = %#v/%v attempts=%v", result, err, adapter.attempts)
	}
	if len(adapter.attempts) != 2 || adapter.attempts[0] != "expired" || adapter.attempts[1] != "fresh" {
		t.Fatalf("attempts = %v, want expired then fresh", adapter.attempts)
	}
}

func TestAdminChatStreamRetriesCredentialRejectedAccountBeforeOutput(t *testing.T) {
	adapter := &accountRetryAdapter{}
	service := NewServiceWithRecorderAndPool(nil, map[string]ports.Adapter{"chatgpt": adapter}, nil, scheduler.NewPool(twoAccountFixture{}, 1, 3))
	chunks := make([]ports.StreamChunk, 0)
	err := service.AdminChatStream(context.Background(), ports.ChatInput{
		Model: "chatgpt/model-chatgpt", Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}},
	}, func(chunk ports.StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil || len(chunks) != 1 || chunks[0].Text != "recovered" {
		t.Fatalf("credential rejection stream retry = %#v/%v attempts=%v", chunks, err, adapter.attempts)
	}
	if len(adapter.attempts) != 2 || adapter.attempts[0] != "expired" || adapter.attempts[1] != "fresh" {
		t.Fatalf("stream attempts = %v, want expired then fresh", adapter.attempts)
	}
}

func TestAdminChatRejectsLocallyDisabledChannel(t *testing.T) {
	service := NewServiceWithRecorderAndPool(nil, map[string]ports.Adapter{
		"wb": &fake.Adapter{Slug: "wb", Reply: "should not be called", Catalogue: []ports.ModelDescriptor{{UpstreamID: "model-a", Capabilities: []string{"chat"}}}},
	}, nil, nil)
	service.SetChannelState(channelStateFixture(false))
	_, err := service.AdminChat(context.Background(), ports.ChatInput{
		Model:    "wb/model-a",
		Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}},
	})
	serviceError, ok := err.(*Error)
	if !ok || serviceError.Code != "channel_disabled" || serviceError.Status != 503 {
		t.Fatalf("disabled channel error = %T/%v", err, err)
	}
}

func TestAdminChatResolvesRouteAliasToPriorityTarget(t *testing.T) {
	service := NewService(nil, map[string]ports.Adapter{
		"wb": &fake.Adapter{Slug: "wb", Reply: "route reply", Catalogue: []ports.ModelDescriptor{{UpstreamID: "model-a", Capabilities: []string{"chat"}}}},
	})
	service.SetRouteSource(routeSourceFixture{})
	result, err := service.AdminChat(context.Background(), ports.ChatInput{
		Model:    "primary",
		Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil || result.Text != "route reply" || result.Channel != "wb" || result.UpstreamModel != "model-a" || result.RouteAlias != "primary" {
		t.Fatalf("route alias result = %#v/%v", result, err)
	}
}

func TestChatRouteFallsBackToNextPriorityTargetOnRetryableError(t *testing.T) {
	service := NewService(nil, map[string]ports.Adapter{
		"first":  retryableAdapter{Adapter: &fake.Adapter{Slug: "first", Catalogue: []ports.ModelDescriptor{{UpstreamID: "model-a", Capabilities: []string{"chat"}}}}},
		"second": &fake.Adapter{Slug: "second", Reply: "fallback reply", Catalogue: []ports.ModelDescriptor{{UpstreamID: "model-b", Capabilities: []string{"chat"}}}},
	})
	service.SetRouteSource(routeSourceFixtureMulti{})
	result, err := service.AdminChat(context.Background(), ports.ChatInput{Model: "fallback", Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}}})
	if err != nil || result.Text != "fallback reply" || result.Channel != "second" || result.UpstreamModel != "model-b" || result.RouteAlias != "fallback" || result.FallbackDepth != 1 {
		t.Fatalf("fallback result = %#v/%v", result, err)
	}
}

func TestTargetErrorCarriesRouteMetadata(t *testing.T) {
	err := annotateTargetError(&Error{Status: 502, Code: "upstream_unavailable"}, modelTarget{Channel: "second", Upstream: "model-b", Alias: "fallback", Depth: 1})
	serviceError, ok := err.(*Error)
	if !ok || serviceError.Channel != "second" || serviceError.UpstreamModel != "model-b" || serviceError.RouteAlias != "fallback" || serviceError.FallbackDepth != 1 {
		t.Fatalf("route error metadata = %#v", err)
	}
}

func TestAdminChatStreamFallsBackBeforeFirstChunk(t *testing.T) {
	service := NewServiceWithRecorderAndPool(nil, map[string]ports.Adapter{
		"first":  streamFixtureAdapter{channel: "first", failBeforeOutput: true},
		"second": streamFixtureAdapter{channel: "second"},
	}, nil, scheduler.NewPool(accountFixture{}, 1, 3))
	service.SetRouteSource(routeSourceFixtureMultiStream{})
	chunks := make([]ports.StreamChunk, 0)
	err := service.AdminChatStream(context.Background(), ports.ChatInput{Model: "stream-fallback", Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}}}, func(chunk ports.StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil || len(chunks) != 1 || chunks[0].Channel != "second" {
		t.Fatalf("pre-output stream fallback = %#v/%v", chunks, err)
	}
}

func TestAdminChatStreamDoesNotReplayAfterFirstChunk(t *testing.T) {
	service := NewServiceWithRecorderAndPool(nil, map[string]ports.Adapter{
		"first":  streamFixtureAdapter{channel: "first", failAfterOutput: true},
		"second": streamFixtureAdapter{channel: "second"},
	}, nil, scheduler.NewPool(accountFixture{}, 1, 3))
	service.SetRouteSource(routeSourceFixtureMultiStream{})
	chunks := make([]ports.StreamChunk, 0)
	err := service.AdminChatStream(context.Background(), ports.ChatInput{Model: "stream-fallback", Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}}}, func(chunk ports.StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err == nil || len(chunks) != 1 || chunks[0].Channel != "first" {
		t.Fatalf("post-output stream replay = %#v/%v", chunks, err)
	}
}

type routeSourceFixtureMultiStream struct{}

func (routeSourceFixtureMultiStream) ListRoutes(context.Context) ([]ports.RouteRecord, error) {
	return []ports.RouteRecord{{Alias: "stream-fallback", Enabled: true, Strategy: "priority", Targets: []ports.RouteTarget{{Channel: "first", Model: "model-first"}, {Channel: "second", Model: "model-second"}}}}, nil
}

type routeSourceFixtureMulti struct{}

func (routeSourceFixtureMulti) ListRoutes(context.Context) ([]ports.RouteRecord, error) {
	return []ports.RouteRecord{{Alias: "fallback", Enabled: true, Strategy: "priority", Targets: []ports.RouteTarget{{Channel: "first", Model: "model-a"}, {Channel: "second", Model: "model-b"}}}}, nil
}
