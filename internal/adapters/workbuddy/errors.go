package workbuddy

import (
	"errors"
	"fmt"
)

var (
	ErrCredentialUnavailable = errors.New("WorkBuddy credential is unavailable")
	ErrProtocol              = errors.New("WorkBuddy response protocol is invalid")
)

type HTTPError struct {
	Status       int
	Operation    string
	ProviderCode string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("WorkBuddy %s endpoint returned HTTP %d", e.Operation, e.Status)
}
func (e *HTTPError) StatusCode() int { return e.Status }
func (e *HTTPError) Kind() string {
	if e.Status == 401 {
		return "credential_rejected"
	}
	if e.Status == 429 {
		return "upstream_rate_limited"
	}
	if e.Status >= 500 {
		return "upstream_unavailable"
	}
	if e.ProviderCode != "" {
		return "upstream_" + e.ProviderCode
	}
	return "upstream_error"
}
