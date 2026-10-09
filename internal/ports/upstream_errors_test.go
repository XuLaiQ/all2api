package ports

import (
	"context"
	"errors"
	"testing"
)

func TestTransportErrorClassifiesUnavailableAndTimeout(t *testing.T) {
	regular := NewTransportError("GET /models", errors.New("connection refused"))
	classified, ok := regular.(interface {
		StatusCode() int
		Kind() string
	})
	if !ok || classified.StatusCode() != 502 || classified.Kind() != "upstream_unavailable" {
		t.Fatalf("regular transport error = %#v", regular)
	}

	timeout := NewTransportError("GET /models", context.DeadlineExceeded)
	classified, ok = timeout.(interface {
		StatusCode() int
		Kind() string
	})
	if !ok || classified.StatusCode() != 504 || classified.Kind() != "upstream_timeout" {
		t.Fatalf("timeout transport error = %#v", timeout)
	}
}
