package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const DefaultMaxCandidates = 3

var ErrNoAvailableAccount = errors.New("no available account")

type Pool struct {
	source        ports.AccountSource
	maxInflight   int
	maxCandidates int
	mu            sync.Mutex
	inflight      map[string]int
	lastSelected  map[string]uint64
	sequence      uint64
}

func NewPool(source ports.AccountSource, maxInflight, maxCandidates int) *Pool {
	if maxInflight < 1 {
		maxInflight = 1
	}
	if maxCandidates < 1 {
		maxCandidates = DefaultMaxCandidates
	}
	return &Pool{source: source, maxInflight: maxInflight, maxCandidates: maxCandidates, inflight: map[string]int{}, lastSelected: map[string]uint64{}}
}

type Lease struct {
	pool      *Pool
	Candidate ports.AccountCandidate
	released  bool
}

func (l *Lease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if count := l.pool.inflight[l.Candidate.ID] - 1; count > 0 {
		l.pool.inflight[l.Candidate.ID] = count
	} else {
		delete(l.pool.inflight, l.Candidate.ID)
	}
}

func (p *Pool) Candidates(ctx context.Context, channel, model string, requireCredentials bool) ([]ports.AccountCandidate, error) {
	if p.source == nil {
		return nil, ErrNoAvailableAccount
	}
	candidates, err := p.source.Candidates(ctx, channel, model, requireCredentials)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ranked := make([]ports.AccountCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if p.inflight[candidate.ID] >= p.maxInflight {
			continue
		}
		ranked = append(ranked, candidate)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := p.lastSelected[ranked[i].ID], p.lastSelected[ranked[j].ID]
		if left == right {
			if ranked[i].Priority == ranked[j].Priority {
				return ranked[i].ID < ranked[j].ID
			}
			return ranked[i].Priority > ranked[j].Priority
		}
		return left < right
	})
	if len(ranked) > p.maxCandidates {
		ranked = ranked[:p.maxCandidates]
	}
	return append([]ports.AccountCandidate(nil), ranked...), nil
}

func (p *Pool) Acquire(ctx context.Context, channel, model string, requireCredentials bool) (*Lease, error) {
	candidates, err := p.Candidates(ctx, channel, model, requireCredentials)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, candidate := range candidates {
		if p.inflight[candidate.ID] >= p.maxInflight {
			continue
		}
		p.inflight[candidate.ID]++
		p.sequence++
		p.lastSelected[candidate.ID] = p.sequence
		return &Lease{pool: p, Candidate: candidate}, nil
	}
	return nil, ErrNoAvailableAccount
}
