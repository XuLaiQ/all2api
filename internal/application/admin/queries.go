package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type QueryService struct{ repository ports.AdminQueryRepository }

func NewQueryService(repository ports.AdminQueryRepository) *QueryService {
	return &QueryService{repository: repository}
}

func (s *QueryService) Logs(ctx context.Context, query ports.LogQuery) ([]ports.RequestLog, int, error) {
	if s.repository == nil {
		return nil, 0, fmt.Errorf("admin query storage is unavailable")
	}
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 || query.PageSize > 200 {
		return nil, 0, fmt.Errorf("page_size must be between 1 and 200")
	}
	return s.repository.ListRequestLogs(ctx, query)
}

func (s *QueryService) Summary(ctx context.Context, days int) (ports.UsageSummary, error) {
	if s.repository == nil {
		return ports.UsageSummary{}, fmt.Errorf("admin query storage is unavailable")
	}
	if days < 1 || days > 366 {
		return ports.UsageSummary{}, fmt.Errorf("days must be between 1 and 366")
	}
	return s.repository.UsageSummary(ctx, days)
}

func (s *QueryService) Rows(ctx context.Context, days int, dimension string) ([]ports.UsageRow, error) {
	if s.repository == nil {
		return nil, fmt.Errorf("admin query storage is unavailable")
	}
	if days < 1 || days > 366 {
		return nil, fmt.Errorf("days must be between 1 and 366")
	}
	switch dimension {
	case "daily", "channel", "model", "key":
	default:
		return nil, fmt.Errorf("unsupported usage dimension")
	}
	return s.repository.UsageRows(ctx, days, dimension)
}

func (s *QueryService) ClearExpiredLogs(ctx context.Context, logRetentionDays, usageRetentionDays int, audit ports.AuditEvent) (ports.ClearLogsResult, error) {
	if s.repository == nil {
		return ports.ClearLogsResult{}, errors.New("admin query storage is unavailable")
	}
	return s.repository.ClearExpiredLogs(ctx, logRetentionDays, usageRetentionDays, audit)
}
