package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) channels(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		data := make([]map[string]any, 0)
		for _, item := range s.registry.List() {
			data = append(data, s.channelView(r.Context(), item))
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"data": data, "total": len(data)})
	case http.MethodPost:
		if user.Role != "admin" {
			writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
			return
		}
		var body struct {
			Slug    string         `json:"slug"`
			Enabled *bool          `json:"enabled"`
			Config  map[string]any `json:"config"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.Slug) == "" {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "slug is required", nil)
			return
		}
		item, exists := s.registry.Get(body.Slug)
		if !exists {
			writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "dynamic provider registration is not implemented", nil)
			return
		}
		if err := validatePublicChannelConfig(body.Config, 0); err != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
			return
		}
		if s.catalog == nil {
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "channel storage is unavailable", nil)
			return
		}
		if _, already, err := s.catalog.GetChannelOverride(r.Context(), body.Slug); err != nil {
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "channel storage is unavailable", nil)
			return
		} else if already {
			writeAdminError(w, r, http.StatusConflict, "conflict", "channel configuration already exists", nil)
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		override, err := s.catalog.UpsertChannelOverride(r.Context(), ports.ChannelOverride{Slug: body.Slug, Name: item.Manifest.Name, Adapter: item.Manifest.Slug, AuthKind: "adapter-managed", Enabled: enabled, Config: body.Config}, ports.AuditEvent{Actor: user.Username, Action: "create_channel_config", IP: s.clientIP(r)})
		if err != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, http.StatusCreated, map[string]any{"data": s.channelViewWithOverride(r.Context(), item, override, true)})
	case http.MethodPatch:
		if user.Role != "admin" {
			writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
			return
		}
		s.channelPatch(w, r, user.Username)
	case http.MethodDelete:
		if user.Role != "admin" {
			writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
			return
		}
		if s.catalog == nil {
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "channel storage is unavailable", nil)
			return
		}
		if _, exists := s.registry.Get(r.PathValue("slug")); !exists {
			writeAdminError(w, r, http.StatusNotFound, "not_found", "channel is not registered", nil)
			return
		}
		if err := s.catalog.DeleteChannelOverride(r.Context(), r.PathValue("slug"), ports.AuditEvent{Actor: user.Username, Action: "delete_channel_config", IP: s.clientIP(r)}); err != nil {
			if errors.Is(err, ports.ErrKeyNotFound) {
				writeAdminError(w, r, http.StatusNotFound, "not_found", "channel configuration not found", nil)
			} else {
				writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", err.Error(), nil)
			}
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"data": map[string]any{"slug": r.PathValue("slug"), "deleted": true, "reset_to_registry_defaults": true}})
	default:
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}

func (s *Server) channelView(ctx context.Context, item ports.AdapterRegistration) map[string]any {
	override := ports.ChannelOverride{}
	hasOverride := false
	if s.catalog != nil {
		if value, exists, err := s.catalog.GetChannelOverride(ctx, item.Manifest.Slug); err == nil {
			override, hasOverride = value, exists
		}
	}
	return s.channelViewWithOverride(ctx, item, override, hasOverride)
}

func (s *Server) channelViewWithOverride(ctx context.Context, item ports.AdapterRegistration, override ports.ChannelOverride, hasOverride bool) map[string]any {
	managementEnabled := true
	config := map[string]any{}
	if hasOverride {
		managementEnabled = override.Enabled
		config = override.Config
	}
	dataPlaneConfigured := item.Adapter != nil
	provisionConfigured := s.provision != nil
	switch item.Manifest.Slug {
	case "wb":
		provisionConfigured = provisionConfigured || s.workbuddyProvision != nil
	case "doubao":
		provisionConfigured = provisionConfigured || s.doubaoProvision != nil
	case "chatgpt":
		provisionConfigured = provisionConfigured || s.chatgptProvision != nil
	case "grok":
		provisionConfigured = provisionConfigured || s.grokProvision != nil
	}
	accountsConfigured := false
	if s.accountSource != nil {
		if candidates, err := s.accountSource.Candidates(ctx, item.Manifest.Slug, "", true); err == nil {
			accountsConfigured = len(candidates) > 0
		}
	}
	state := "ready"
	if !dataPlaneConfigured {
		state = "not_configured"
	} else if !managementEnabled {
		state = "disabled"
	}
	return map[string]any{
		"slug": item.Manifest.Slug, "name": item.Manifest.Name, "adapter": item.Manifest.Slug,
		"adapter_version": item.Manifest.Version, "enabled": dataPlaneConfigured && managementEnabled,
		"management_enabled": managementEnabled, "data_plane_configured": dataPlaneConfigured,
		"state": state, "accounts_configured": accountsConfigured, "provision_configured": provisionConfigured,
		"account_config": map[string]any{"configured": provisionConfigured, "required_env": []string{}, "missing_env": []string{}},
		"config":         config, "protocols": item.Manifest.Protocols, "caps": item.Manifest.Capabilities,
		"runtime": map[string]any{"state": "closed", "consecutive_failures": 0, "cooldown_until": nil, "breaker_until": nil, "retry_after": nil},
	}
}

func (s *Server) channelPatch(w http.ResponseWriter, r *http.Request, actor string) {
	if s.catalog == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "channel storage is unavailable", nil)
		return
	}
	slug := r.PathValue("slug")
	item, exists := s.registry.Get(slug)
	if !exists {
		writeAdminError(w, r, http.StatusNotFound, "not_found", "channel is not registered", nil)
		return
	}
	var body struct {
		Enabled *bool           `json:"enabled"`
		Config  *map[string]any `json:"config"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || (body.Enabled == nil && body.Config == nil) {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "at least one channel field is required", nil)
		return
	}
	current, hasCurrent, err := s.catalog.GetChannelOverride(r.Context(), slug)
	if err != nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "channel storage is unavailable", nil)
		return
	}
	if !hasCurrent {
		current = ports.ChannelOverride{Slug: slug, Name: item.Manifest.Name, Adapter: item.Manifest.Slug, AuthKind: "adapter-managed", Enabled: item.Adapter != nil, Config: map[string]any{}}
	}
	if body.Enabled != nil {
		current.Enabled = *body.Enabled
	}
	if body.Config != nil {
		if err := validatePublicChannelConfig(*body.Config, 0); err != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
			return
		}
		current.Config = *body.Config
	}
	updated, err := s.catalog.UpsertChannelOverride(r.Context(), current, ports.AuditEvent{Actor: actor, Action: "update_channel_config", IP: s.clientIP(r)})
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": s.channelViewWithOverride(r.Context(), item, updated, true)})
}

func validatePublicChannelConfig(value any, depth int) error {
	if depth > 4 {
		return fmt.Errorf("channel config nesting is too deep")
	}
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) > 64 {
			return fmt.Errorf("channel config has too many fields")
		}
		for key, child := range typed {
			name := strings.TrimSpace(key)
			if name == "" || len(name) > 64 {
				return fmt.Errorf("channel config key is invalid")
			}
			normalized := strings.ToLower(strings.ReplaceAll(name, "-", "_"))
			for _, sensitive := range []string{"token", "cookie", "password", "secret", "authorization", "api_key", "apikey", "private_key"} {
				if strings.Contains(normalized, sensitive) {
					return fmt.Errorf("provider secret field is not accepted: config.%s", name)
				}
			}
			if err := validatePublicChannelConfig(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if len(typed) > 64 {
			return fmt.Errorf("channel config list is too long")
		}
		for _, child := range typed {
			if err := validatePublicChannelConfig(child, depth+1); err != nil {
				return err
			}
		}
	case nil, bool, float64, string:
		if typedString, ok := typed.(string); ok && len(typedString) > 4096 {
			return fmt.Errorf("channel config value is too long")
		}
	default:
		return fmt.Errorf("unsupported channel config value")
	}
	return nil
}

func (s *Server) channelRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	item, ok := s.registry.Get(r.PathValue("slug"))
	if !ok {
		writeAdminError(w, r, 404, "not_found", "channel is not registered", nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"channel": item.Manifest.Slug, "states": []any{}}})
}

func (s *Server) channelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	item, ok := s.registry.Get(r.PathValue("slug"))
	if !ok {
		writeAdminError(w, r, 404, "not_found", "channel is not registered", nil)
		return
	}
	if item.Adapter == nil || s.accountSource == nil {
		writeAdminError(w, r, 409, "channel_unconfigured", "channel has no usable local adapter or account source", nil)
		return
	}
	candidates, err := s.accountSource.Candidates(r.Context(), item.Manifest.Slug, "", true)
	if err != nil || len(candidates) == 0 {
		writeAdminError(w, r, 409, "channel_unconfigured", "channel has no usable local account", nil)
		return
	}
	started := time.Now()
	var models []ports.ModelDescriptor
	if accountAdapter, ok := item.Adapter.(ports.AccountModelAdapter); ok {
		models, err = accountAdapter.ModelsWithAccount(r.Context(), candidates[0].NativeID)
	} else {
		models, err = item.Adapter.Models(r.Context())
	}
	if err != nil {
		writeAdminError(w, r, 502, "adapter_error", "channel test failed", nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"channel": item.Manifest.Slug, "status": "ok", "latency_ms": time.Since(started).Milliseconds(), "model_count": len(models), "tested_at": time.Now().UTC().Format(time.RFC3339)}})
}

func (s *Server) channelAdapters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	data := make([]map[string]any, 0)
	for _, item := range s.registry.List() {
		data = append(data, map[string]any{
			"slug": item.Manifest.Slug, "name": item.Manifest.Name,
			"version": item.Manifest.Version, "protocols": item.Manifest.Protocols,
			"capabilities": item.Manifest.Capabilities, "configured": item.Adapter != nil,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": data})
}
