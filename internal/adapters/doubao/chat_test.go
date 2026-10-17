package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func newTestClient(url string) *Client {
	return NewClient(url, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x; passport_csrf_token=csrf-1"}, nil
	})
}

func TestDoubaoCompletionStreamsThinkingAndText(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completion" || r.Header.Get("x-tt-passport-csrf-token") != "csrf-1" || r.Header.Get("agw-js-conv") != "str" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		frames := []string{
			"event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"123\"}}\n\n",
			"event: STREAM_CHUNK\ndata: {\"patch_op\":[{\"patch_value\":{\"content_block\":[{\"block_type\":10040,\"content\":{}}]}}]}\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"think-1 \"}\n\n",
			"event: STREAM_CHUNK\ndata: {\"patch_op\":[{\"patch_value\":{\"content_block\":[{\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"think-2\"}}}]}}]}\n\n",
			"event: STREAM_CHUNK\ndata: {\"patch_op\":[{\"patch_value\":{\"content_block\":[{\"block_type\":10040,\"content\":{}}]}}]}\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"ans\"}\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"ans\"}\n\n",
			"event: STREAM_CHUNK\ndata: {\"patch_op\":[{\"patch_value\":{\"content\":\"{\\\"text\\\":\\\"!\\\"}\"}}]}\n\n",
			"event: SSE_REPLY_END\ndata: {}\n\n",
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte(frame))
		}
	}))
	defer server.Close()
	client := newTestClient(server.URL)
	chunks := 0
	var result ports.ChatResult
	err := client.ChatStream(context.Background(), ports.ChatInput{Model: "doubao-think", Messages: []ports.ChatMessage{
		{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"},
	}}, "account", func(chunk ports.StreamChunk) error {
		chunks++
		result.Text += chunk.Text
		result.Reasoning += chunk.Reasoning
		if chunk.ID == "" {
			t.Fatalf("chunk without id")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if result.Reasoning != "think-1 think-2" || result.Text != "ansans!" || chunks != 5 {
		t.Fatalf("result = %#v (%d chunks)", result, chunks)
	}
	option, _ := payload["option"].(map[string]any)
	if option["need_deep_think"] != float64(1) {
		t.Fatalf("need_deep_think = %v", option["need_deep_think"])
	}
	messages, _ := payload["messages"].([]any)
	message, _ := messages[0].(map[string]any)
	blocks, _ := message["content_block"].([]any)
	block, _ := blocks[0].(map[string]any)
	content, _ := block["content"].(map[string]any)
	textBlock, _ := content["text_block"].(map[string]any)
	if textBlock["text"] != "[system]: be brief\n[user]: hi" {
		t.Fatalf("prompt = %q", textBlock["text"])
	}
}

func TestDoubaoCompletionExpiredLoginJSONDoesNotFallBack(t *testing.T) {
	var aliceCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/alice/") {
			atomic.AddInt32(&aliceCalls, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":710012001,"msg":"expired"}`))
	}))
	defer server.Close()
	_, err := newTestClient(server.URL).Chat(context.Background(), ports.ChatInput{Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized || httpErr.Code != "710012001" {
		t.Fatalf("Chat() error = %T/%v", err, err)
	}
	if atomic.LoadInt32(&aliceCalls) != 0 {
		t.Fatalf("alice fallback must not run for expired credentials")
	}
}

func TestDoubaoCompletionGatewayErrorIsCredentialRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: gateway-error\ndata: {\"code\":4011,\"message\":\"user invalid\"}\n\n"))
	}))
	defer server.Close()
	_, err := newTestClient(server.URL).Chat(context.Background(), ports.ChatInput{Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Kind() != "credential_rejected" {
		t.Fatalf("Chat() error = %T/%v", err, err)
	}
}

func TestDoubaoCompletionRiskErrorFallsBackToAlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Path == "/chat/completion" {
			_, _ = w.Write([]byte("event: STREAM_ERROR\ndata: {\"error_code\":710022004,\"error_msg\":\"verify\",\"extra\":{\"decision\":\"{}\"}}\n\n"))
			return
		}
		if r.URL.Path != "/alice/message/stream_call_bot" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		frames := []string{
			`{"event_type":2001,"message":{"reply_id":"0","content_type":1,"content":"{\"text\":\"echo\"}"}}`,
			`{"event_type":2001,"message":{"reply_id":"9","content_type":1,"content":"{\"text\":\"he\"}"}}`,
			`{"event_type":2001,"message":{"reply_id":"9","content_type":1,"content":"{\"text\":\"llo\"}"}}`,
			`{"event_type":2001,"message":{"reply_id":"9","content_type":2,"content":"{\"suggest\":[\"next\"]}"}}`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
		}
		_, _ = w.Write([]byte("event: done\ndata: {}\n\n"))
	}))
	defer server.Close()
	result, err := newTestClient(server.URL).Chat(context.Background(), ports.ChatInput{Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	if err != nil || result.Text != "hello" {
		t.Fatalf("Chat() = %#v, %v", result, err)
	}
}

func TestDoubaoCompletionRiskErrorReturnedWhenFallbackFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completion" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: STREAM_ERROR\ndata: {\"error_code\":710022002,\"error_msg\":\"limited\"}\n\n"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	_, err := newTestClient(server.URL).Chat(context.Background(), ports.ChatInput{Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusTooManyRequests || httpErr.Code != "710022002" {
		t.Fatalf("Chat() error = %T/%v", err, err)
	}
}

func TestDoubaoDeepThinkLevel(t *testing.T) {
	cases := map[string]int{"doubao": 0, "doubao-pro": 0, "doubao-think": 1, "doubao-expert": 3, "Doubao-Expert": 3, "upstream-key": 0}
	for model, want := range cases {
		if got := deepThinkLevel(model); got != want {
			t.Fatalf("deepThinkLevel(%q) = %d, want %d", model, got, want)
		}
	}
	if got := flattenPrompt([]ports.ChatMessage{{Role: "user", Content: "only"}}); got != "only" {
		t.Fatalf("single prompt = %q", got)
	}
}
