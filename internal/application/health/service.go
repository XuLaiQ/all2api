package health

import (
	"context"
	"fmt"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type Service struct {
	storage ports.Storage
}

func NewService(storage ports.Storage) *Service {
	return &Service{storage: storage}
}

func (s *Service) Check(ctx context.Context, detail bool) (map[string]any, error) {
	if err := s.storage.Ping(ctx); err != nil {
		return nil, fmt.Errorf("database unavailable: %w", err)
	}

	result := map[string]any{
		"status":   "ok",
		"service":  "all2api-api",
		"database": "ok",
	}
	if !detail {
		return result, nil
	}
	snapshot, err := s.storage.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("database snapshot unavailable: %w", err)
	}
	result["checks"] = map[string]any{
		"storage": map[string]any{
			"status":         "ready",
			"schema_version": snapshot.SchemaVersion,
			"tables":         snapshot.Tables,
		},
	}
	return result, nil
}
