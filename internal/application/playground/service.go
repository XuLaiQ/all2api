package playground

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type Service struct{ repository ports.PlaygroundRepository }

func NewService(repository ports.PlaygroundRepository) *Service {
	return &Service{repository: repository}
}

func (s *Service) ListConversations(ctx context.Context, actor string, page, size int) ([]ports.PlaygroundConversationSummary, int, error) {
	if s.repository == nil {
		return nil, 0, errors.New("playground storage is unavailable")
	}
	return s.repository.ListPlaygroundConversations(ctx, actor, page, size)
}
func (s *Service) GetConversation(ctx context.Context, actor, id string) (ports.PlaygroundConversation, error) {
	if s.repository == nil {
		return ports.PlaygroundConversation{}, errors.New("playground storage is unavailable")
	}
	return s.repository.GetPlaygroundConversation(ctx, actor, id)
}
func (s *Service) DeleteConversation(ctx context.Context, actor, id string, audit ports.AuditEvent) (int, error) {
	if s.repository == nil {
		return 0, errors.New("playground storage is unavailable")
	}
	return s.repository.DeletePlaygroundConversation(ctx, actor, id, audit)
}
func (s *Service) ListRuns(ctx context.Context, page, size int) ([]ports.PlaygroundRun, int, error) {
	if s.repository == nil {
		return nil, 0, errors.New("playground storage is unavailable")
	}
	return s.repository.ListPlaygroundRuns(ctx, page, size)
}
func (s *Service) CreateConversation(ctx context.Context, item ports.PlaygroundConversationSummary) error {
	if s.repository == nil {
		return errors.New("playground storage is unavailable")
	}
	return s.repository.CreatePlaygroundConversation(ctx, item)
}
func (s *Service) AddMessage(ctx context.Context, conversationID string, item ports.PlaygroundMessage) error {
	if s.repository == nil {
		return errors.New("playground storage is unavailable")
	}
	return s.repository.AddPlaygroundMessage(ctx, conversationID, item)
}
func (s *Service) StartRun(ctx context.Context, item ports.PlaygroundRun) error {
	if s.repository == nil {
		return errors.New("playground storage is unavailable")
	}
	if item.ID == "" {
		return fmt.Errorf("run id is required")
	}
	return s.repository.CreatePlaygroundRun(ctx, item)
}
func (s *Service) FinishRun(ctx context.Context, id, status string, responseStatus int, code string) error {
	if s.repository == nil {
		return errors.New("playground storage is unavailable")
	}
	return s.repository.FinishPlaygroundRun(ctx, id, status, responseStatus, code, time.Now())
}
