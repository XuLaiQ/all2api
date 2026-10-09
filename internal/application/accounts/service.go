package accounts

import (
	"context"
	"errors"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type Service struct{ repository ports.AccountRepository }

func NewService(repository ports.AccountRepository) *Service { return &Service{repository: repository} }

func (s *Service) List(ctx context.Context, query ports.AccountQuery) ([]ports.AccountRecord, int, error) {
	if s.repository == nil {
		return nil, 0, errors.New("account storage is unavailable")
	}
	return s.repository.ListAccounts(ctx, query)
}

func (s *Service) SetEnabled(ctx context.Context, id string, enabled bool, audit ports.AuditEvent) (ports.AccountRecord, error) {
	if s.repository == nil {
		return ports.AccountRecord{}, errors.New("account storage is unavailable")
	}
	return s.repository.SetAccountEnabled(ctx, id, enabled, audit)
}

func (s *Service) Delete(ctx context.Context, id string, audit ports.AuditEvent) error {
	if s.repository == nil {
		return errors.New("account storage is unavailable")
	}
	return s.repository.DeleteAccount(ctx, id, audit)
}

func (s *Service) BatchDelete(ctx context.Context, ids []string, audit ports.AuditEvent) (ports.AccountBatchDeleteResult, error) {
	if s.repository == nil {
		return ports.AccountBatchDeleteResult{}, errors.New("account storage is unavailable")
	}
	return s.repository.DeleteAccounts(ctx, ids, audit)
}
