package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type SystemService struct{ repository ports.SystemRepository }

func NewSystemService(repository ports.SystemRepository) *SystemService {
	return &SystemService{repository: repository}
}

func (s *SystemService) Info(ctx context.Context, statePath string, channels []ports.SystemChannel) (ports.SystemInfo, error) {
	if s.repository == nil {
		return ports.SystemInfo{}, errors.New("system storage is unavailable")
	}
	return s.repository.SystemInfo(ctx, statePath, channels)
}

func (s *SystemService) Storage(ctx context.Context, statePath string) (ports.StorageHealth, error) {
	if s.repository == nil {
		return ports.StorageHealth{}, errors.New("system storage is unavailable")
	}
	return s.repository.StorageHealth(ctx, statePath)
}

func (s *SystemService) Metrics(ctx context.Context, days int) (ports.SystemMetrics, error) {
	if s.repository == nil {
		return ports.SystemMetrics{}, errors.New("system storage is unavailable")
	}
	if days < 1 || days > 366 {
		return ports.SystemMetrics{}, fmt.Errorf("days must be between 1 and 366")
	}
	return s.repository.Metrics(ctx, days)
}
