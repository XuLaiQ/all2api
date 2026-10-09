package admin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type UserService struct{ repository ports.UserRepository }

func NewUserService(repository ports.UserRepository) *UserService {
	return &UserService{repository: repository}
}

func (s *UserService) List(ctx context.Context) ([]ports.UserRecord, error) {
	if s.repository == nil {
		return nil, errors.New("user storage is unavailable")
	}
	return s.repository.ListUsers(ctx)
}

func (s *UserService) Create(ctx context.Context, username, role string, enabled bool, audit ports.AuditEvent) (ports.UserRecord, error) {
	if err := validateUser(username, role); err != nil {
		return ports.UserRecord{}, err
	}
	now := time.Now().Unix()
	return s.repository.CreateUser(ctx, ports.UserRecord{Username: username, Role: role, Enabled: enabled, CreatedAt: now, UpdatedAt: now}, audit)
}

func (s *UserService) Update(ctx context.Context, username string, role *string, enabled *bool, audit ports.AuditEvent) (ports.UserRecord, error) {
	if username == "" {
		return ports.UserRecord{}, fmt.Errorf("username is required")
	}
	users, err := s.repository.ListUsers(ctx)
	if err != nil {
		return ports.UserRecord{}, err
	}
	var current ports.UserRecord
	for _, item := range users {
		if item.Username == username {
			current = item
			break
		}
	}
	if current.Username == "" {
		return ports.UserRecord{}, ports.ErrKeyNotFound
	}
	if role != nil {
		if err := validateRole(*role); err != nil {
			return ports.UserRecord{}, err
		}
		current.Role = *role
	}
	if enabled != nil {
		current.Enabled = *enabled
	}
	return s.repository.UpdateUser(ctx, username, current, audit)
}

func (s *UserService) Delete(ctx context.Context, username string, audit ports.AuditEvent) error {
	if strings.EqualFold(username, "admin") {
		return fmt.Errorf("configured admin cannot be deleted")
	}
	return s.repository.DeleteUser(ctx, username, audit)
}

func validateUser(username, role string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("invalid username")
	}
	return validateRole(role)
}

func validateRole(role string) error {
	if role != "admin" && role != "viewer" {
		return fmt.Errorf("role must be admin or viewer")
	}
	return nil
}
