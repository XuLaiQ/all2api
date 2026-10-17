package httptransport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	stored, err := s.catalog.Settings(r.Context())
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	logDays := s.config.LogRetentionDays
	usageDays := s.config.UsageRetentionDays
	if value, exists := stored["log_retention_days"]; exists {
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
			logDays = parsed
		}
	}
	if value, exists := stored["usage_retention_days"]; exists {
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
			usageDays = parsed
		}
	}
	if r.Method == http.MethodGet {
		writeJSON(w, r, http.StatusOK, map[string]any{"data": map[string]any{"values": map[string]int{"log_retention_days": logDays, "usage_retention_days": usageDays}, "sources": map[string]string{"log_retention_days": sourceForSetting(stored, "log_retention_days"), "usage_retention_days": sourceForSetting(stored, "usage_retention_days")}, "mutable": []string{"log_retention_days", "usage_retention_days"}, "restart_required": false}})
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
		return
	}
	values := map[string]string{}
	for key, value := range body {
		number, ok := value.(float64)
		if !ok || number < 1 || number > 36500 || number != float64(int(number)) {
			writeAdminError(w, r, 422, "validation_error", "retention values must be positive integers", nil)
			return
		}
		values[key] = strconv.Itoa(int(number))
	}
	if next, exists := values["log_retention_days"]; exists {
		logDays, _ = strconv.Atoi(next)
	}
	if next, exists := values["usage_retention_days"]; exists {
		usageDays, _ = strconv.Atoi(next)
	}
	if usageDays < logDays {
		writeAdminError(w, r, 422, "validation_error", "usage retention must be >= log retention", nil)
		return
	}
	if _, err := s.catalog.UpdateSettings(r.Context(), values, ports.AuditEvent{Actor: user.Username, Action: "update_settings", IP: s.clientIP(r)}); err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": map[string]any{"values": map[string]int{"log_retention_days": logDays, "usage_retention_days": usageDays}, "sources": map[string]string{"log_retention_days": "database", "usage_retention_days": "database"}, "mutable": []string{"log_retention_days", "usage_retention_days"}, "restart_required": false}})
}

func sourceForSetting(stored map[string]string, key string) string {
	if _, ok := stored[key]; ok {
		return "database"
	}
	return "environment"
}

func (s *Server) usersRoot(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	if s.users == nil {
		writeAdminError(w, r, 503, "service_unavailable", "user storage is unavailable", nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.users.List(r.Context())
		if err != nil {
			writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": rows})
	case http.MethodPost:
		if user.Role != "admin" {
			writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
			return
		}
		var body struct {
			Username string `json:"username"`
			Role     string `json:"role"`
			Enabled  bool   `json:"enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		created, err := s.users.Create(r.Context(), body.Username, body.Role, body.Enabled, ports.AuditEvent{Actor: user.Username, Action: "create_user", IP: s.clientIP(r)})
		if err != nil {
			writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 201, map[string]any{"data": created})
	default:
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
	}
}

func (s *Server) userItem(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	if s.users == nil {
		writeAdminError(w, r, 503, "service_unavailable", "user storage is unavailable", nil)
		return
	}
	username := r.PathValue("username")
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Role    *string `json:"role"`
			Enabled *bool   `json:"enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		updated, err := s.users.Update(r.Context(), username, body.Role, body.Enabled, ports.AuditEvent{Actor: user.Username, Action: "update_user", IP: s.clientIP(r)})
		if err != nil {
			writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": updated})
	case http.MethodDelete:
		if err := s.users.Delete(r.Context(), username, ports.AuditEvent{Actor: user.Username, Action: "delete_user", IP: s.clientIP(r)}); err != nil {
			if errors.Is(err, ports.ErrKeyNotFound) {
				writeAdminError(w, r, 404, "not_found", "user not found", nil)
			} else {
				writeAdminError(w, r, 409, "conflict", err.Error(), nil)
			}
			return
		}
		w.WriteHeader(204)
	default:
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
	}
}

func (s *Server) sysinfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	channels := make([]ports.SystemChannel, 0)
	for _, item := range s.registry.List() {
		view := s.channelView(r.Context(), item)
		modelsConfigured, _ := view["data_plane_configured"].(bool)
		accountsConfigured, _ := view["accounts_configured"].(bool)
		provisionConfigured, _ := view["provision_configured"].(bool)
		channels = append(channels, ports.SystemChannel{Slug: item.Manifest.Slug, ModelsConfigured: modelsConfigured, AccountsConfigured: accountsConfigured, ProvisionConfigured: provisionConfigured})
	}
	value, err := s.system.Info(r.Context(), s.config.StatePath, channels)
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": value})
}

func (s *Server) storageHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	value, err := s.system.Storage(r.Context(), s.config.StatePath)
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": value})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	days := 1
	if value := r.URL.Query().Get("days"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeAdminError(w, r, 422, "validation_error", "days must be an integer", nil)
			return
		}
		days = parsed
	}
	value, err := s.system.Metrics(r.Context(), days)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": value})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	days := 30
	if value := r.URL.Query().Get("days"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeAdminError(w, r, 422, "validation_error", "days must be an integer", nil)
			return
		}
		days = parsed
	}
	summary, err := s.adminQueries.Summary(r.Context(), days)
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	daily, err := s.adminQueries.Rows(r.Context(), days, "daily")
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	channels := make([]map[string]any, 0)
	for _, item := range s.registry.List() {
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
			if candidates, candidateErr := s.accountSource.Candidates(r.Context(), item.Manifest.Slug, "", true); candidateErr == nil {
				accountsConfigured = len(candidates) > 0
			}
		}
		channels = append(channels, map[string]any{"slug": item.Manifest.Slug, "name": item.Manifest.Name, "adapter": item.Manifest.Slug, "enabled": true, "state": map[bool]string{true: "ready", false: "not_configured"}[item.Adapter != nil], "accounts_configured": accountsConfigured, "provision_configured": provisionConfigured, "protocols": item.Manifest.Protocols, "caps": item.Manifest.Capabilities, "runtime": map[string]any{"state": "closed", "consecutive_failures": 0, "cooldown_until": nil, "breaker_until": nil, "retry_after": nil}})
	}
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"summary": summary, "daily": map[string]any{"data": daily, "from": summary.From, "to": summary.To}, "recent": map[string]any{"metric": "tokens", "usage_semantics": "reported_or_estimated_tokens", "bucket": "hour", "from": summary.From, "to": summary.To, "series": []any{}}, "channels": channels, "todos": []any{}}})
}
