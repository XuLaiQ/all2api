package httptransport

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	page, pageSize, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
		return
	}
	var status *int
	if value := r.URL.Query().Get("status"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "status must be an integer", nil)
			return
		}
		status = &parsed
	}
	var stream *bool
	if value := r.URL.Query().Get("stream"); value != "" {
		parsed, parseErr := strconv.ParseBool(value)
		if parseErr != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "stream must be a boolean", nil)
			return
		}
		stream = &parsed
	}
	from, err := parseLogTime(r.URL.Query().Get("from"))
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "from must be RFC 3339", nil)
		return
	}
	to, err := parseLogTime(r.URL.Query().Get("to"))
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "to must be RFC 3339", nil)
		return
	}
	if from != nil && to != nil && *from >= *to {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "from must be before to", nil)
		return
	}
	rows, total, err := s.adminQueries.Logs(r.Context(), ports.LogQuery{Page: page, PageSize: pageSize, RequestID: r.URL.Query().Get("request_id"), Channel: r.URL.Query().Get("channel"), Model: r.URL.Query().Get("model"), Status: status, ErrorKind: r.URL.Query().Get("error_kind"), Stream: stream, From: from, To: to})
	if err != nil {
		writeAdminError(w, r, http.StatusInternalServerError, "internal_error", "could not read request logs", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": rows, "pagination": map[string]int{"page": page, "page_size": pageSize, "total": total, "total_pages": (total + pageSize - 1) / pageSize}})
}

func parseLogTime(value string) (*int64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	result := parsed.Unix()
	return &result, nil
}

func (s *Server) logsClear(w http.ResponseWriter, r *http.Request) {
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
	if s.catalog == nil || s.adminQueries == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "log storage is unavailable", nil)
		return
	}
	stored, err := s.catalog.Settings(r.Context())
	if err != nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "retention settings are unavailable", nil)
		return
	}
	logDays, usageDays := s.config.LogRetentionDays, s.config.UsageRetentionDays
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
	result, err := s.adminQueries.ClearExpiredLogs(r.Context(), logDays, usageDays, ports.AuditEvent{Actor: user.Username, Action: "clear_logs", IP: s.clientIP(r)})
	if err != nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "could not clear expired logs", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": result})
}

func (s *Server) auditLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	if s.auditRepository == nil {
		writeAdminError(w, r, 503, "service_unavailable", "audit storage is unavailable", nil)
		return
	}
	page, size, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	rows, total, err := s.auditRepository.ListAudit(r.Context(), ports.AuditQuery{Page: page, PageSize: size, Actor: r.URL.Query().Get("actor"), Action: r.URL.Query().Get("action"), Target: r.URL.Query().Get("target")})
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, map[string]any{"id": row.ID, "ts": time.Unix(row.Timestamp, 0).UTC().Format(time.RFC3339), "actor": row.Actor, "action": row.Action, "target": row.Target, "detail": row.Detail})
	}
	writeJSON(w, r, 200, map[string]any{"data": data, "pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size}})
}

func (s *Server) statsSummary(w http.ResponseWriter, r *http.Request) {
	s.stats(w, r, "summary")
}

func (s *Server) statsDaily(w http.ResponseWriter, r *http.Request) {
	s.stats(w, r, "daily")
}

func (s *Server) statsChannel(w http.ResponseWriter, r *http.Request) {
	s.stats(w, r, "channel")
}

func (s *Server) statsModel(w http.ResponseWriter, r *http.Request) {
	s.stats(w, r, "model")
}

func (s *Server) statsKey(w http.ResponseWriter, r *http.Request) {
	s.stats(w, r, "key")
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request, dimension string) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	days := 30
	if value := r.URL.Query().Get("days"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "days must be an integer", nil)
			return
		}
		days = parsed
	}
	if dimension == "summary" {
		value, err := s.adminQueries.Summary(r.Context(), days)
		if err != nil {
			writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"data": value})
		return
	}
	rows, err := s.adminQueries.Rows(r.Context(), days, dimension)
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": rows})
}
