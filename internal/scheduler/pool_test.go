package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type sourceFixture struct{ candidates []ports.AccountCandidate }

func (s sourceFixture) Candidates(context.Context, string, string, bool) ([]ports.AccountCandidate, error) {
	return append([]ports.AccountCandidate(nil), s.candidates...), nil
}

func TestPoolLimitsInflightAndReleasesIdempotently(t *testing.T) {
	pool := NewPool(sourceFixture{candidates: []ports.AccountCandidate{{ID: "a", NativeID: "cn:a", Priority: 1}, {ID: "b", NativeID: "cn:b", Priority: 0}}}, 1, 3)
	first, err := pool.Acquire(context.Background(), "wb", "model-a", true)
	if err != nil || first.Candidate.ID != "a" {
		t.Fatalf("first Acquire() = %#v, %v", first, err)
	}
	second, err := pool.Acquire(context.Background(), "wb", "model-a", true)
	if err != nil || second.Candidate.ID != "b" {
		t.Fatalf("second Acquire() = %#v, %v", second, err)
	}
	if _, err := pool.Acquire(context.Background(), "wb", "model-a", true); !errors.Is(err, ErrNoAvailableAccount) {
		t.Fatalf("third Acquire() error = %v", err)
	}
	first.Release()
	first.Release()
	third, err := pool.Acquire(context.Background(), "wb", "model-a", true)
	if err != nil || third.Candidate.ID != "a" {
		t.Fatalf("released Acquire() = %#v, %v", third, err)
	}
	third.Release()
}
