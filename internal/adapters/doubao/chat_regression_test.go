package doubao

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestDoubaoDoesNotRetryAccessRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name string
		status int
		frame string
		wantStatus int
	}{
		{"forbidden", 403, "", 403},
		{"http_rate_limit", 429, "", 429},
		{"verification", 200, "event: STREAM_ERROR\ndata: {\"error_code\":710022004}\n\n", 429},
		{"rate_limit", 200, "event: STREAM_ERROR\ndata: {\"error_code\":710022002}\n\n", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.frame)
			}))
			defer server.Close()
			_, err := newTestClient(server.URL).Chat(context.Background(), ports.ChatInput{Messages: []ports.ChatMessage{{Role: "user", Content: "test"}}}, "account")
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != tc.wantStatus {
				t.Fatalf("error = %v, want status %d", err, tc.wantStatus)
			}
			if requests.Load() != 1 {
				t.Fatalf("access restriction caused %d requests; want exactly one", requests.Load())
			}
		})
	}
}

func TestDoubaoStreamErrorAfterTextIsNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: CHUNK_DELTA\ndata: {\"text\":\"partial\"}\n\nevent: STREAM_ERROR\ndata: {\"error_code\":99999}\n\n")
	}))
	defer server.Close()
	err := newTestClient(server.URL).ChatStream(context.Background(), ports.ChatInput{Messages: []ports.ChatMessage{{Role: "user", Content: "test"}}}, "account", func(ports.StreamChunk) error { return nil })
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != "99999" {
		t.Fatalf("partial failed stream reported success: %v", err)
	}
}

func TestDoubaoCompletionStopsAtReplyEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: CHUNK_DELTA\ndata: {\"text\":\"complete\"}\n\nevent: SSE_REPLY_END\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		// A terminal event must close the request even if the connection stays open.
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*1e9)
	defer cancel()
	result, err := newTestClient(server.URL).Chat(ctx, ports.ChatInput{Messages: []ports.ChatMessage{{Role: "user", Content: "test"}}}, "account")
	if err != nil || result.Text != "complete" {
		t.Fatalf("terminal event result = %q, %v", result.Text, err)
	}
}
