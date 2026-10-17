package httptransport

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	authapp "github.com/XuLaiQ/all2api/internal/application/auth"
	keyapp "github.com/XuLaiQ/all2api/internal/application/keys"
	"github.com/XuLaiQ/all2api/internal/domain/scope"
	"github.com/XuLaiQ/all2api/internal/ports"
)

type createKeyRequest struct {
	Name      string   `json:"name"`
	Channels  []string `json:"channels"`
	Models    []string `json:"models"`
	ExpiresAt *int64   `json:"expires_at"`
	LimitRPM  int      `json:"limit_rpm"`
}

func (s *Server) keysRoot(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "key storage is unavailable", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listKeys(w, r)
	case http.MethodPost:
		if user.Role != "admin" {
			writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
			return
		}
		s.createKey(w, r, user)
	default:
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}

func (s *Server) keyItem(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "key storage is unavailable", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok || user.Role != "admin" {
		if ok {
			writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
		}
		return
	}
	id, err := strconv.ParseInt(r.PathValue("key_id"), 10, 64)
	if err != nil || id < 1 {
		writeAdminError(w, r, http.StatusNotFound, "not_found", "API key not found", nil)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		s.updateKey(w, r, user, id)
	case http.MethodDelete:
		s.revokeKey(w, r, user, id)
	default:
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}

func (s *Server) keyRotate(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "key storage is unavailable", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok || user.Role != "admin" {
		if ok {
			writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
		}
		return
	}
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("key_id"), 10, 64)
	if err != nil || id < 1 {
		writeAdminError(w, r, http.StatusNotFound, "not_found", "API key not found", nil)
		return
	}
	key, rawKey, err := s.keys.Rotate(r.Context(), id, user.Username, s.clientIP(r))
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{"data": keyPayload(key), "key": rawKey})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
		return
	}
	filter := ports.KeyListFilter{
		Page: page, PageSize: pageSize, Search: r.URL.Query().Get("search"),
	}
	if value := r.URL.Query().Get("enabled"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "enabled must be a boolean", nil)
			return
		}
		filter.Enabled = &enabled
	}
	if len(filter.Search) > 128 {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "search is too long", nil)
		return
	}
	rows, total, err := s.keys.List(r.Context(), filter)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{
		"data": rowsPayload(rows),
		"pagination": map[string]int{
			"page": page, "page_size": pageSize, "total": total,
			"total_pages": (total + pageSize - 1) / pageSize,
		},
	})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, user authapp.User) {
	var body createKeyRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "request validation failed", nil)
		return
	}
	key, rawKey, err := s.keys.Create(r.Context(), keyapp.CreateInput{
		Name: body.Name, Channels: body.Channels, Models: body.Models, ExpiresAt: body.ExpiresAt,
		LimitRPM: body.LimitRPM, Actor: user.Username, IP: s.clientIP(r),
	})
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusCreated, map[string]any{"data": keyPayload(key), "key": rawKey})
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request, user authapp.User, id int64) {
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&raw); err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "request validation failed", nil)
		return
	}
	input, err := decodePatchKey(raw)
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
		return
	}
	input.Actor = user.Username
	input.IP = s.clientIP(r)
	key, err := s.keys.Update(r.Context(), id, input)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": keyPayload(key)})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, user authapp.User, id int64) {
	if err := s.keys.Revoke(r.Context(), id, user.Username, s.clientIP(r)); err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeKeyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ports.ErrKeyNotFound):
		writeAdminError(w, r, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, ports.ErrKeyNameExists):
		writeAdminError(w, r, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, ports.ErrBootstrapKey), errors.Is(err, keyapp.ErrBootstrap):
		writeAdminError(w, r, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, keyapp.ErrNoFields):
		writeAdminError(w, r, http.StatusBadRequest, "bad_request", err.Error(), nil)
	case errors.Is(err, keyapp.ErrNameInvalid), errors.Is(err, keyapp.ErrExpirationPast):
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
	case errors.Is(err, ports.ErrInvalidKey):
		writeAdminError(w, r, http.StatusUnauthorized, "unauthorized", err.Error(), nil)
	default:
		var validation *scope.ValidationError
		if errors.As(err, &validation) {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", validation.Error(), nil)
			return
		}
		if s.logger != nil {
			s.logger.Printf("key operation failed request_id=%s error=%v", requestID(r.Context()), err)
		}
		writeAdminError(w, r, http.StatusInternalServerError, "internal_error", "internal server error", nil)
	}
}

func keyPayload(key ports.APIKey) map[string]any {
	return map[string]any{
		"id": key.ID, "name": key.Name, "prefix": key.Prefix, "enabled": key.Enabled,
		"expires_at": key.ExpiresAt, "channels": key.Channels, "models": key.Models,
		"limit_rpm": key.LimitRPM, "created_at": key.CreatedAt, "last_used_at": key.LastUsedAt,
	}
}

func rowsPayload(rows []ports.APIKey) []map[string]any {
	result := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		result = append(result, keyPayload(row))
	}
	return result
}

func decodePatchKey(raw map[string]json.RawMessage) (keyapp.PatchInput, error) {
	allowed := map[string]bool{"name": true, "enabled": true, "channels": true, "models": true, "expires_at": true, "limit_rpm": true}
	for field := range raw {
		if !allowed[field] {
			return keyapp.PatchInput{}, fmt.Errorf("unsupported key field: %s", field)
		}
	}
	result := keyapp.PatchInput{}
	if value, ok := raw["name"]; ok {
		if string(value) == "null" {
			return result, errors.New("name cannot be null")
		}
		if err := json.Unmarshal(value, &result.Name); err != nil {
			return result, errors.New("name must be a string")
		}
	}
	if value, ok := raw["enabled"]; ok {
		if string(value) == "null" {
			return result, errors.New("enabled cannot be null")
		}
		if err := json.Unmarshal(value, &result.Enabled); err != nil {
			return result, errors.New("enabled must be a boolean")
		}
	}
	if value, ok := raw["channels"]; ok {
		if string(value) == "null" {
			return result, errors.New("channels cannot be null")
		}
		var channels []string
		if json.Unmarshal(value, &channels) != nil {
			return result, errors.New("channels must be an array")
		}
		result.Channels = &channels
	}
	if value, ok := raw["models"]; ok {
		if string(value) == "null" {
			return result, errors.New("models cannot be null")
		}
		var models []string
		if json.Unmarshal(value, &models) != nil {
			return result, errors.New("models must be an array")
		}
		result.Models = &models
	}
	if value, ok := raw["expires_at"]; ok {
		result.ExpiresAtSet = true
		if string(value) != "null" {
			var expires int64
			if json.Unmarshal(value, &expires) != nil {
				return result, errors.New("expires_at must be an integer or null")
			}
			result.ExpiresAt = &expires
		}
	}
	if value, ok := raw["limit_rpm"]; ok {
		if string(value) == "null" {
			return result, errors.New("limit_rpm cannot be null")
		}
		if err := json.Unmarshal(value, &result.LimitRPM); err != nil {
			return result, errors.New("limit_rpm must be an integer")
		}
	}
	return result, nil
}
