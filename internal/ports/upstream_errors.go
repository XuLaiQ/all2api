package ports

import (
	"context"
	"errors"
	"net"
)

// TransportError keeps network failures classifiable without exposing URLs,
// proxy details, or upstream response bodies to the gateway or its logs.
type TransportError struct {
	Operation string
	Err       error
}

func NewTransportError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return &TransportError{Operation: operation, Err: err}
}

func (e *TransportError) Error() string { return "upstream transport request failed" }

func (e *TransportError) Unwrap() error { return e.Err }

func (e *TransportError) StatusCode() int {
	if isTimeout(e.Err) {
		return 504
	}
	return 502
}

func (e *TransportError) Kind() string {
	if isTimeout(e.Err) {
		return "upstream_timeout"
	}
	return "upstream_unavailable"
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
