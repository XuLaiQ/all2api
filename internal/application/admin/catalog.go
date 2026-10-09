package admin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/XuLaiQ/all2api/internal/ports"
)

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type CatalogService struct{ repository ports.CatalogRepository }

func NewCatalogService(repository ports.CatalogRepository) *CatalogService {
	return &CatalogService{repository: repository}
}

func (s *CatalogService) Models(ctx context.Context, query ports.ModelQuery) ([]ports.ModelRecord, int, []string, error) {
	if s.repository == nil {
		return nil, 0, nil, errors.New("catalog storage is unavailable")
	}
	return s.repository.ListModels(ctx, query)
}

func (s *CatalogService) SyncModels(ctx context.Context, channel string, models []ports.ModelDescriptor, audit ports.AuditEvent) (int, error) {
	repository, ok := s.repository.(ports.ModelSyncRepository)
	if !ok {
		return 0, errors.New("catalog synchronization is unavailable")
	}
	return repository.SyncModels(ctx, channel, models, audit)
}

func (s *CatalogService) SetModelEnabled(ctx context.Context, id string, enabled bool, audit ports.AuditEvent) (ports.ModelRecord, error) {
	if s.repository == nil {
		return ports.ModelRecord{}, errors.New("catalog storage is unavailable")
	}
	return s.repository.UpdateModelEnabled(ctx, id, enabled, audit)
}

func (s *CatalogService) Routes(ctx context.Context) ([]ports.RouteRecord, error) {
	if s.repository == nil {
		return nil, errors.New("catalog storage is unavailable")
	}
	return s.repository.ListRoutes(ctx)
}

func (s *CatalogService) SaveRoute(ctx context.Context, alias string, input ports.RouteInput, audit ports.AuditEvent) (ports.RouteRecord, error) {
	if s.repository == nil {
		return ports.RouteRecord{}, errors.New("catalog storage is unavailable")
	}
	if !aliasPattern.MatchString(alias) {
		return ports.RouteRecord{}, fmt.Errorf("invalid route alias")
	}
	if input.Strategy == "" {
		input.Strategy = "priority"
	}
	if input.Strategy != "priority" || len(input.Targets) == 0 || len(input.Targets) > 8 {
		return ports.RouteRecord{}, fmt.Errorf("invalid route input")
	}
	seen := map[string]bool{}
	for _, target := range input.Targets {
		if strings.TrimSpace(target.Channel) == "" || strings.TrimSpace(target.Model) == "" || seen[target.Channel] {
			return ports.RouteRecord{}, fmt.Errorf("route targets must use distinct non-empty channels")
		}
		if strings.Contains(target.Model, "/") && !strings.HasPrefix(target.Model, target.Channel+"/") {
			return ports.RouteRecord{}, fmt.Errorf("target model prefix must match its channel")
		}
		seen[target.Channel] = true
	}
	return s.repository.UpsertRoute(ctx, alias, input, audit)
}

func (s *CatalogService) DeleteRoute(ctx context.Context, alias string, audit ports.AuditEvent) error {
	if s.repository == nil {
		return errors.New("catalog storage is unavailable")
	}
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("invalid route alias")
	}
	return s.repository.DeleteRoute(ctx, alias, audit)
}

func (s *CatalogService) Settings(ctx context.Context) (map[string]string, error) {
	if s.repository == nil {
		return nil, errors.New("catalog storage is unavailable")
	}
	return s.repository.GetSettings(ctx)
}

func (s *CatalogService) UpdateSettings(ctx context.Context, values map[string]string, audit ports.AuditEvent) (map[string]string, error) {
	if s.repository == nil {
		return nil, errors.New("catalog storage is unavailable")
	}
	for key, value := range values {
		if key != "log_retention_days" && key != "usage_retention_days" {
			return nil, fmt.Errorf("unsupported setting: %s", key)
		}
		if value == "" {
			return nil, fmt.Errorf("setting %s must not be empty", key)
		}
	}
	return values, s.repository.PutSettings(ctx, values, audit)
}

func (s *CatalogService) GetChannelOverride(ctx context.Context, slug string) (ports.ChannelOverride, bool, error) {
	repository, ok := s.repository.(ports.ChannelRepository)
	if !ok {
		return ports.ChannelOverride{}, false, errors.New("channel storage is unavailable")
	}
	return repository.GetChannelOverride(ctx, slug)
}

func (s *CatalogService) UpsertChannelOverride(ctx context.Context, item ports.ChannelOverride, audit ports.AuditEvent) (ports.ChannelOverride, error) {
	repository, ok := s.repository.(ports.ChannelRepository)
	if !ok {
		return ports.ChannelOverride{}, errors.New("channel storage is unavailable")
	}
	if !aliasPattern.MatchString(item.Slug) {
		return ports.ChannelOverride{}, errors.New("invalid channel slug")
	}
	if item.Config == nil {
		item.Config = map[string]any{}
	}
	return repository.UpsertChannelOverride(ctx, item, audit)
}

func (s *CatalogService) DeleteChannelOverride(ctx context.Context, slug string, audit ports.AuditEvent) error {
	repository, ok := s.repository.(ports.ChannelRepository)
	if !ok {
		return errors.New("channel storage is unavailable")
	}
	return repository.DeleteChannelOverride(ctx, slug, audit)
}
