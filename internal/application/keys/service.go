package keys

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/XuLaiQ/all2api/internal/domain/scope"
	"github.com/XuLaiQ/all2api/internal/ports"
)

var namePattern = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,128}$`)

var (
	ErrNameInvalid    = errors.New("name contains unsupported characters")
	ErrNoFields       = errors.New("no key fields were provided")
	ErrExpirationPast = errors.New("expires_at must be in the future")
	ErrBootstrap      = ports.ErrBootstrapKey
)

type CreateInput struct {
	Name      string
	Channels  []string
	Models    []string
	ExpiresAt *int64
	LimitRPM  int
	Actor     string
	IP        string
}

type PatchInput struct {
	Name         *string
	Enabled      *bool
	Channels     *[]string
	Models       *[]string
	ExpiresAt    *int64
	ExpiresAtSet bool
	LimitRPM     *int
	Actor        string
	IP           string
}

type Service struct {
	repository    ports.KeyRepository
	secretBox     ports.SecretBox
	material      ports.KeyMaterial
	knownChannels func() []string
	now           func() time.Time
}

func NewService(repository ports.KeyRepository, secretBox ports.SecretBox, material ports.KeyMaterial, knownChannels func() []string) *Service {
	return &Service{
		repository:    repository,
		secretBox:     secretBox,
		material:      material,
		knownChannels: knownChannels,
		now:           time.Now,
	}
}

func (s *Service) List(ctx context.Context, filter ports.KeyListFilter) ([]ports.APIKey, int, error) {
	return s.repository.ListKeys(ctx, filter)
}

func (s *Service) Create(ctx context.Context, input CreateInput) (ports.APIKey, string, error) {
	name, err := validateName(input.Name)
	if err != nil {
		return ports.APIKey{}, "", err
	}
	if strings.EqualFold(name, "bootstrap") {
		return ports.APIKey{}, "", ErrBootstrap
	}
	if err := validateExpiry(input.ExpiresAt, s.now()); err != nil {
		return ports.APIKey{}, "", err
	}
	channels, models, err := s.validateScope(input.Channels, input.Models)
	if err != nil {
		return ports.APIKey{}, "", err
	}
	if input.LimitRPM < 0 || input.LimitRPM > 1_000_000 {
		return ports.APIKey{}, "", fmt.Errorf("limit_rpm is outside the allowed range")
	}
	if s.secretBox == nil {
		return ports.APIKey{}, "", errors.New("credential encryption is not configured")
	}
	if s.material == nil {
		return ports.APIKey{}, "", errors.New("API key material is not configured")
	}
	rawKey, err := s.material.Issue()
	if err != nil {
		return ports.APIKey{}, "", err
	}
	encrypted, err := s.secretBox.Encrypt(rawKey)
	if err != nil {
		return ports.APIKey{}, "", fmt.Errorf("encrypt API key: %w", err)
	}
	key, err := s.repository.CreateKey(ctx, ports.KeyCreate{
		Name:      name,
		KeyHash:   s.material.Hash(rawKey),
		Encrypted: encrypted,
		Prefix:    rawKey[:14],
		ExpiresAt: input.ExpiresAt,
		Channels:  channels,
		Models:    models,
		LimitRPM:  input.LimitRPM,
		CreatedAt: s.now().Unix(),
		Audit: ports.AuditEvent{
			Actor: input.Actor, Action: "key.created", IP: trimIP(input.IP),
		},
	})
	if err != nil {
		return ports.APIKey{}, "", err
	}
	return key, rawKey, nil
}

func (s *Service) Update(ctx context.Context, id int64, input PatchInput) (ports.APIKey, error) {
	if input.Name == nil && input.Enabled == nil && input.Channels == nil && input.Models == nil && !input.ExpiresAtSet && input.LimitRPM == nil {
		return ports.APIKey{}, ErrNoFields
	}
	current, err := s.repository.GetKey(ctx, id)
	if err != nil {
		return ports.APIKey{}, err
	}
	if strings.EqualFold(current.Name, "bootstrap") {
		return ports.APIKey{}, ErrBootstrap
	}
	patch := ports.KeyPatch{
		Enabled:      input.Enabled,
		ExpiresAt:    input.ExpiresAt,
		ExpiresAtSet: input.ExpiresAtSet,
		LimitRPM:     input.LimitRPM,
	}
	if input.Name != nil {
		name, err := validateName(*input.Name)
		if err != nil {
			return ports.APIKey{}, err
		}
		if strings.EqualFold(name, "bootstrap") {
			return ports.APIKey{}, ErrBootstrap
		}
		patch.Name = &name
	}
	channels := current.Channels
	if input.Channels != nil {
		channels = *input.Channels
		patch.Channels = &channels
	}
	models := current.Models
	if input.Models != nil {
		models = *input.Models
		patch.Models = &models
	}
	normalizedChannels, normalizedModels, err := s.validateScope(channels, models)
	if err != nil {
		return ports.APIKey{}, err
	}
	if input.Channels != nil {
		patch.Channels = &normalizedChannels
	}
	if input.Models != nil {
		patch.Models = &normalizedModels
	}
	if input.ExpiresAtSet {
		if err := validateExpiry(input.ExpiresAt, s.now()); err != nil {
			return ports.APIKey{}, err
		}
	}
	if input.LimitRPM != nil && (*input.LimitRPM < 0 || *input.LimitRPM > 1_000_000) {
		return ports.APIKey{}, fmt.Errorf("limit_rpm is outside the allowed range")
	}
	patch.Audit = ports.AuditEvent{
		Actor: input.Actor, Action: "key.updated", Detail: updateDetail(input), IP: trimIP(input.IP),
	}
	return s.repository.UpdateKey(ctx, id, patch)
}

func (s *Service) Rotate(ctx context.Context, id int64, actor, ip string) (ports.APIKey, string, error) {
	current, err := s.repository.GetKey(ctx, id)
	if err != nil {
		return ports.APIKey{}, "", err
	}
	if strings.EqualFold(current.Name, "bootstrap") {
		return ports.APIKey{}, "", ErrBootstrap
	}
	if s.secretBox == nil || s.material == nil {
		return ports.APIKey{}, "", errors.New("credential encryption is not configured")
	}
	rawKey, err := s.material.Issue()
	if err != nil {
		return ports.APIKey{}, "", err
	}
	encrypted, err := s.secretBox.Encrypt(rawKey)
	if err != nil {
		return ports.APIKey{}, "", fmt.Errorf("encrypt API key: %w", err)
	}
	key, err := s.repository.RotateKey(ctx, id, s.material.Hash(rawKey), encrypted, rawKey[:14], ports.AuditEvent{
		Actor: actor, Action: "key.rotated", IP: trimIP(ip),
	})
	if err != nil {
		return ports.APIKey{}, "", err
	}
	return key, rawKey, nil
}

func (s *Service) Revoke(ctx context.Context, id int64, actor, ip string) error {
	key, err := s.repository.GetKey(ctx, id)
	if err != nil {
		return err
	}
	if strings.EqualFold(key.Name, "bootstrap") {
		return ErrBootstrap
	}
	return s.repository.RevokeKey(ctx, id, ports.AuditEvent{Actor: actor, Action: "key.revoked", IP: trimIP(ip)})
}

func (s *Service) Authenticate(ctx context.Context, authorization, xAPIKey string) (ports.APIKey, error) {
	rawKey, err := parseAPIKey(authorization, xAPIKey)
	if err != nil {
		return ports.APIKey{}, err
	}
	if s.material == nil {
		return ports.APIKey{}, errors.New("API key material is not configured")
	}
	return s.repository.AuthenticateKey(ctx, s.material.Hash(rawKey), s.now())
}

func (s *Service) validateScope(channels, models []string) ([]string, []string, error) {
	known := []string{}
	if s.knownChannels != nil {
		known = s.knownChannels()
	}
	return scope.Validate(channels, models, known)
}

func validateName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !namePattern.MatchString(value) {
		return "", ErrNameInvalid
	}
	for _, char := range value {
		if !unicode.IsPrint(char) || char == '\u007f' {
			return "", ErrNameInvalid
		}
	}
	return value, nil
}

func validateExpiry(value *int64, now time.Time) error {
	if value != nil && *value <= now.Unix() {
		return ErrExpirationPast
	}
	return nil
}

func parseAPIKey(authorization, xAPIKey string) (string, error) {
	authorization = strings.TrimSpace(authorization)
	xAPIKey = strings.TrimSpace(xAPIKey)
	if authorization != "" {
		scheme, value, ok := strings.Cut(authorization, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(value) == "" {
			return "", ports.ErrInvalidKey
		}
		value = strings.TrimSpace(value)
		if xAPIKey != "" && !constantTimeEqual(value, xAPIKey) {
			return "", ports.ErrInvalidKey
		}
		return value, nil
	}
	if xAPIKey == "" {
		return "", ports.ErrInvalidKey
	}
	return xAPIKey, nil
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func updateDetail(input PatchInput) string {
	fields := make([]string, 0, 6)
	if input.Name != nil {
		fields = append(fields, "name")
	}
	if input.Enabled != nil {
		fields = append(fields, "enabled")
	}
	if input.Channels != nil {
		fields = append(fields, "channels")
	}
	if input.Models != nil {
		fields = append(fields, "models")
	}
	if input.ExpiresAtSet {
		fields = append(fields, "expires_at")
	}
	if input.LimitRPM != nil {
		fields = append(fields, "limit_rpm")
	}
	return strings.Join(fields, ",")
}

func trimIP(value string) string {
	if len(value) > 64 {
		return value[:64]
	}
	return value
}
