package ports

import "context"

type AccountCandidate struct {
	ID       string
	NativeID string
	Channel  string
	Priority int
}

type AccountSource interface {
	Candidates(context.Context, string, string, bool) ([]AccountCandidate, error)
}
