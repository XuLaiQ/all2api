package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	page, pageSize, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	var enabled *bool
	if value := r.URL.Query().Get("enabled"); value != "" {
		parsed, parseErr := strconv.ParseBool(value)
		if parseErr != nil {
			writeAdminError(w, r, 422, "validation_error", "enabled must be a boolean", nil)
			return
		}
		enabled = &parsed
	}
	rows, total, kinds, err := s.catalog.Models(r.Context(), ports.ModelQuery{Page: page, PageSize: pageSize, Channel: r.URL.Query().Get("channel"), Kind: r.URL.Query().Get("kind"), Search: r.URL.Query().Get("search"), Enabled: enabled})
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": rows, "pagination": map[string]int{"page": page, "page_size": pageSize, "total": total, "total_pages": (total + pageSize - 1) / pageSize}, "source": "observed_cache", "facets": map[string]any{"kinds": kinds}})
}

func (s *Server) adminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
		return
	}
	if s.catalog == nil || s.modelSync == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "catalog synchronization is unavailable", nil)
		return
	}
	requested := strings.TrimSpace(r.URL.Query().Get("channel"))
	if requested != "" {
		if _, ok := s.registry.Get(requested); !ok {
			writeAdminError(w, r, http.StatusNotFound, "not_found", "channel adapter is not registered", nil)
			return
		}
	}
	channels := make([]string, 0)
	if requested != "" {
		channels = append(channels, requested)
	} else {
		for _, item := range s.registry.List() {
			channels = append(channels, item.Manifest.Slug)
		}
	}
	results := make([]map[string]any, 0, len(channels))
	total := 0
	synced := false
	for _, channel := range channels {
		item, exists := s.registry.Get(channel)
		if !exists || item.Adapter == nil {
			results = append(results, map[string]any{"channel": channel, "status": "failed", "error": "adapter_unavailable"})
			continue
		}
		models, err := s.liveModels(r.Context(), channel, item.Adapter)
		if err != nil {
			results = append(results, map[string]any{"channel": channel, "status": "failed", "error": "upstream_unavailable"})
			continue
		}
		for index := range models {
			models[index].Channel = channel
		}
		count, err := s.catalog.SyncModels(r.Context(), channel, models, ports.AuditEvent{Actor: user.Username, Action: "refresh_models", IP: s.clientIP(r)})
		if err != nil {
			results = append(results, map[string]any{"channel": channel, "status": "failed", "error": "catalog_unavailable"})
			continue
		}
		synced = true
		total += count
		results = append(results, map[string]any{"channel": channel, "status": "ok", "count": count})
	}
	if synced && s.gateway != nil {
		s.gateway.InvalidateModelMappings()
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": map[string]any{"source": "live", "refreshed_at": time.Now().Unix(), "total": total, "channels": results}})
}

func (s *Server) liveModels(ctx context.Context, channel string, adapter ports.Adapter) ([]ports.ModelDescriptor, error) {
	if s.accountPool != nil {
		candidates, err := s.accountPool.Candidates(ctx, channel, "", true)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return nil, errors.New("no account is available")
		}
		if accountAdapter, ok := adapter.(ports.AccountModelAdapter); ok {
			lease, err := s.accountPool.Acquire(ctx, channel, "", true)
			if err != nil {
				return nil, err
			}
			models, callErr := accountAdapter.ModelsWithAccount(ctx, lease.Candidate.NativeID)
			lease.Release()
			return models, callErr
		}
	}
	return adapter.Models(ctx)
}

func (s *Server) adminModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
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
	var body struct {
		Enabled     *bool   `json:"enabled"`
		DisplayName *string `json:"display_name"`
		PublicID    *string `json:"public_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
		return
	}
	item, err := s.catalog.UpdateModel(r.Context(), r.PathValue("model_id"), ports.ModelUpdate{Enabled: body.Enabled, DisplayName: body.DisplayName, PublicID: body.PublicID}, ports.AuditEvent{Actor: user.Username, Action: "update_model", IP: s.clientIP(r)})
	if errors.Is(err, ports.ErrKeyNotFound) {
		writeAdminError(w, r, 404, "not_found", "model not found", nil)
		return
	}
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	if s.gateway != nil {
		s.gateway.InvalidateModelMappings()
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": item})
}

func (s *Server) adminRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	rows, err := s.catalog.Routes(r.Context())
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": rows, "total": len(rows)})
}

func (s *Server) adminRoute(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	alias := r.PathValue("alias")
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Strategy string              `json:"strategy"`
			Enabled  bool                `json:"enabled"`
			Targets  []ports.RouteTarget `json:"targets"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		item, err := s.catalog.SaveRoute(r.Context(), alias, ports.RouteInput{Strategy: body.Strategy, Enabled: body.Enabled, Targets: body.Targets}, ports.AuditEvent{Actor: user.Username, Action: "upsert_route", IP: s.clientIP(r)})
		if err != nil {
			writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"data": item})
	case http.MethodDelete:
		if err := s.catalog.DeleteRoute(r.Context(), alias, ports.AuditEvent{Actor: user.Username, Action: "delete_route", IP: s.clientIP(r)}); err != nil {
			if errors.Is(err, ports.ErrKeyNotFound) {
				writeAdminError(w, r, 404, "not_found", "route not found", nil)
			} else {
				writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}
