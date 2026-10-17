package httptransport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	grokadapter "github.com/XuLaiQ/all2api/internal/adapters/grok"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) accountsRoot(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	if s.accounts == nil {
		writeAdminError(w, r, 503, "service_unavailable", "account storage is unavailable", nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		page, size, err := parsePagination(r)
		if err != nil {
			writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
			return
		}
		rows, total, err := s.accounts.List(r.Context(), ports.AccountQuery{Page: page, PageSize: size, Channel: r.URL.Query().Get("channel"), Status: r.URL.Query().Get("status"), Search: r.URL.Query().Get("search")})
		if err != nil {
			writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": rows, "pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size}, "unconfigured_channels": []string{}})
	case http.MethodPost:
		if user.Role != "admin" {
			writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
			return
		}
		writeAdminError(w, r, 501, "capability_not_supported", "account provisioning is not enabled in this Go build", nil)
	default:
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
	}
}

func (s *Server) accountsBatchDelete(w http.ResponseWriter, r *http.Request) {
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
	if s.accounts == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "account storage is unavailable", nil)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || len(body.IDs) == 0 {
		writeAdminError(w, r, http.StatusBadRequest, "invalid_request", "ids is required", nil)
		return
	}
	result, err := s.accounts.BatchDelete(r.Context(), body.IDs, ports.AuditEvent{Actor: user.Username, Action: "delete_account", IP: s.clientIP(r)})
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": result})
}

func (s *Server) accountItem(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	if s.accounts == nil {
		writeAdminError(w, r, 503, "service_unavailable", "account storage is unavailable", nil)
		return
	}
	id := r.PathValue("account_id")
	switch r.Method {
	case http.MethodPost:
		if strings.HasPrefix(id, "wb:") && s.workbuddyProvision != nil {
			nativeID := strings.TrimPrefix(id, "wb:")
			result, err := s.workbuddyProvision.Refresh(r.Context(), nativeID, user.Username, s.clientIP(r))
			if err != nil {
				writeAdminError(w, r, http.StatusBadGateway, "credential_refresh_failed", "provider credential refresh failed", nil)
				return
			}
			writeJSON(w, r, http.StatusOK, map[string]any{"data": result})
			return
		}
		if strings.HasPrefix(id, "grok:") && s.grokProvision != nil {
			result, err := s.grokProvision.Refresh(r.Context(), strings.TrimPrefix(id, "grok:"), user.Username, s.clientIP(r))
			if err != nil {
				message := "provider credential refresh failed"
				var oauthErr *grokadapter.OAuthError
				if errors.As(err, &oauthErr) && oauthErr.Permanent() {
					message = "refresh token is invalid or revoked; sign the account in again"
				}
				writeAdminError(w, r, http.StatusBadGateway, "credential_refresh_failed", message, nil)
				return
			}
			writeJSON(w, r, http.StatusOK, map[string]any{"data": result})
			return
		}
		if !strings.HasPrefix(id, "chatgpt:") || s.chatgptProvision == nil {
			writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "credential refresh is not enabled for this account", nil)
			return
		}
		nativeID := strings.TrimPrefix(id, "chatgpt:")
		if !strings.HasPrefix(nativeID, "oauth:") && !strings.HasPrefix(nativeID, "token:") {
			nativeID = "oauth:" + nativeID
		}
		result, err := s.chatgptProvision.Refresh(r.Context(), nativeID, user.Username, s.clientIP(r))
		if err != nil {
			writeAdminError(w, r, http.StatusBadGateway, "credential_refresh_failed", "provider credential refresh failed", nil)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"data": result})
	case http.MethodPatch:
		var body struct {
			Enabled bool `json:"enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "enabled must be a boolean", nil)
			return
		}
		item, err := s.accounts.SetEnabled(r.Context(), id, body.Enabled, ports.AuditEvent{Actor: user.Username, Action: "set_account_enabled", IP: s.clientIP(r)})
		if err != nil {
			if errors.Is(err, ports.ErrKeyNotFound) {
				writeAdminError(w, r, 404, "not_found", "account not found", nil)
			} else {
				writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
			}
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": item})
	case http.MethodDelete:
		if err := s.accounts.Delete(r.Context(), id, ports.AuditEvent{Actor: user.Username, Action: "delete_account", IP: s.clientIP(r)}); err != nil {
			if errors.Is(err, ports.ErrKeyNotFound) {
				writeAdminError(w, r, 404, "not_found", "account not found", nil)
			} else {
				writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
			}
			return
		}
		w.WriteHeader(204)
	default:
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
	}
}

func (s *Server) provisionSchema(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	channel := r.PathValue("channel")
	if channel != "wb" && channel != "doubao" && channel != "chatgpt" && channel != "grok" {
		writeAdminError(w, r, 404, "not_found", "channel provision schema not found", nil)
		return
	}
	_, configured := s.registry.Get(channel)
	properties := map[string]any{"accounts": map[string]any{"type": "array"}}
	if channel == "doubao" {
		properties["cookie"] = map[string]any{"type": "string", "format": "textarea", "secret": true}
	}
	if channel == "chatgpt" {
		properties["tokens"] = map[string]any{"type": "array", "secret": true}
	}
	if channel == "grok" {
		properties["tokens"] = map[string]any{"type": "array", "secret": true, "description": "Grok Web SSO tokens"}
		properties["accounts"] = map[string]any{"type": "array", "items": map[string]any{"type": "object"}}
	}
	flows := []any{map[string]any{"id": "token-import", "kind": "token_import", "schema": map[string]any{"type": "object", "properties": properties, "required": []string{}, "additionalProperties": true}, "supports": map[string]bool{"import": true}, "timeout_seconds": 300}}
	if channel == "wb" && s.workbuddyProvision != nil {
		flows = append(flows, map[string]any{"id": "qr-oauth", "kind": "oauth", "schema": map[string]any{"type": "object", "properties": map[string]any{"realm": map[string]any{"type": "string", "enum": []string{"cn", "global"}, "default": "cn"}}, "additionalProperties": false}, "supports": map[string]bool{"start": true, "poll": true, "complete": true, "cancel": true}, "timeout_seconds": 300})
	}
	if channel == "doubao" && s.doubaoProvision != nil {
		flows = append(flows, map[string]any{"id": "qr-login", "kind": "qr", "schema": map[string]any{"type": "object", "properties": map[string]any{"account_id": map[string]any{"type": "string"}}, "additionalProperties": false}, "supports": map[string]bool{"start": true, "poll": true, "complete": true, "cancel": true}, "timeout_seconds": 600})
	}
	if channel == "grok" && s.grokProvision != nil {
		flows = append(flows, map[string]any{"id": grokadapter.DeviceFlow, "kind": "oauth", "schema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, "supports": map[string]bool{"start": true, "poll": true, "cancel": true}, "timeout_seconds": 1800})
	}
	if channel == "chatgpt" && s.chatgptProvision != nil {
		flows = append(flows, map[string]any{"id": "oauth-pkce", "kind": "oauth", "schema": map[string]any{"type": "object", "properties": map[string]any{"email_hint": map[string]any{"type": "string"}}, "additionalProperties": false}, "supports": map[string]bool{"start": true, "complete": true, "cancel": true}, "timeout_seconds": 600})
	}
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"channel": channel, "adapter_version": "0.1.0", "configured": configured, "flows": flows}})
}

func (s *Server) provisionImport(w http.ResponseWriter, r *http.Request) {
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
	if s.provision == nil {
		writeAdminError(w, r, 503, "service_unavailable", "provision storage is unavailable", nil)
		return
	}
	var body struct {
		Flow           string         `json:"flow"`
		Payload        map[string]any `json:"payload"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
		return
	}
	if body.Flow != "token-import" {
		writeAdminError(w, r, 422, "validation_error", "only token-import is available in this build", nil)
		return
	}
	result, err := s.provision.Import(r.Context(), r.PathValue("channel"), body.Payload, user.Username, s.clientIP(r), body.IdempotencyKey)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"status": "success", "channel": r.PathValue("channel"), "flow": body.Flow, "idempotency_key": body.IdempotencyKey, "added": result.Added, "updated": result.Updated, "skipped": result.Skipped, "errors": result.Errors, "accounts": result.Accounts}})
}

func (s *Server) provisionStart(w http.ResponseWriter, r *http.Request) {
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
	if r.PathValue("channel") == "doubao" && s.doubaoProvision != nil {
		var body struct {
			Flow           string         `json:"flow"`
			Payload        map[string]any `json:"payload"`
			IdempotencyKey string         `json:"idempotency_key"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		result, err := s.doubaoProvision.Start(r.Context(), body.Flow, body.Payload, body.IdempotencyKey)
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if r.PathValue("channel") == "grok" && s.grokProvision != nil {
		var body struct {
			Flow           string         `json:"flow"`
			Payload        map[string]any `json:"payload"`
			IdempotencyKey string         `json:"idempotency_key"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		result, err := s.grokProvision.Start(r.Context(), body.Flow, body.Payload, body.IdempotencyKey)
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if r.PathValue("channel") == "chatgpt" && s.chatgptProvision != nil {
		var body struct {
			Flow           string         `json:"flow"`
			Payload        map[string]any `json:"payload"`
			IdempotencyKey string         `json:"idempotency_key"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		result, err := s.chatgptProvision.Start(r.Context(), body.Flow, body.Payload, body.IdempotencyKey)
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if s.workbuddyProvision == nil || r.PathValue("channel") != "wb" {
		writeAdminError(w, r, 501, "capability_not_supported", "channel provision is not enabled in this Go build", nil)
		return
	}
	var body struct {
		Flow           string         `json:"flow"`
		Payload        map[string]any `json:"payload"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
		return
	}
	result, err := s.workbuddyProvision.Start(r.Context(), body.Flow, body.Payload, body.IdempotencyKey)
	if err != nil {
		writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": result})
}

func (s *Server) provisionPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	if r.PathValue("channel") == "doubao" && s.doubaoProvision != nil {
		result, err := s.doubaoProvision.Poll(r.Context(), r.PathValue("session_id"))
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if r.PathValue("channel") == "grok" && s.grokProvision != nil {
		result, err := s.grokProvision.Poll(r.Context(), r.PathValue("session_id"))
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if r.PathValue("channel") == "chatgpt" && s.chatgptProvision != nil {
		result, err := s.chatgptProvision.Poll(r.Context(), r.PathValue("session_id"))
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if s.workbuddyProvision == nil || r.PathValue("channel") != "wb" {
		writeAdminError(w, r, 501, "capability_not_supported", "channel provision is not enabled in this Go build", nil)
		return
	}
	result, err := s.workbuddyProvision.Poll(r.Context(), r.PathValue("session_id"))
	if err != nil {
		writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": result})
}

func (s *Server) provisionComplete(w http.ResponseWriter, r *http.Request) {
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
	if r.PathValue("channel") == "doubao" && s.doubaoProvision != nil {
		var body struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		result, err := s.doubaoProvision.Complete(r.Context(), r.PathValue("session_id"), body.IdempotencyKey)
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if r.PathValue("channel") == "chatgpt" && s.chatgptProvision != nil {
		var body struct {
			IdempotencyKey string         `json:"idempotency_key"`
			Payload        map[string]any `json:"payload"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
			return
		}
		result, err := s.chatgptProvision.Complete(r.Context(), r.PathValue("session_id"), body.IdempotencyKey, body.Payload)
		if err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": result})
		return
	}
	if s.workbuddyProvision == nil || r.PathValue("channel") != "wb" {
		writeAdminError(w, r, 501, "capability_not_supported", "channel provision is not enabled in this Go build", nil)
		return
	}
	var body struct {
		IdempotencyKey string         `json:"idempotency_key"`
		Payload        map[string]any `json:"payload"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, 422, "validation_error", "request body is invalid", nil)
		return
	}
	_ = user
	result, err := s.workbuddyProvision.Complete(r.Context(), r.PathValue("session_id"), body.IdempotencyKey)
	if err != nil {
		writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": result})
}

func (s *Server) provisionCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	if r.PathValue("channel") == "doubao" && s.doubaoProvision != nil {
		if err := s.doubaoProvision.Cancel(r.Context(), r.PathValue("session_id")); err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": map[string]any{"channel": "doubao", "session_id": r.PathValue("session_id"), "status": "cancelled"}})
		return
	}
	if r.PathValue("channel") == "grok" && s.grokProvision != nil {
		if err := s.grokProvision.Cancel(r.Context(), r.PathValue("session_id")); err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": map[string]any{"channel": "grok", "session_id": r.PathValue("session_id"), "status": "cancelled"}})
		return
	}
	if r.PathValue("channel") == "chatgpt" && s.chatgptProvision != nil {
		if err := s.chatgptProvision.Cancel(r.Context(), r.PathValue("session_id")); err != nil {
			writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": map[string]any{"channel": "chatgpt", "session_id": r.PathValue("session_id"), "status": "cancelled"}})
		return
	}
	if s.workbuddyProvision == nil || r.PathValue("channel") != "wb" {
		writeAdminError(w, r, 501, "capability_not_supported", "channel provision is not enabled in this Go build", nil)
		return
	}
	if err := s.workbuddyProvision.Cancel(r.Context(), r.PathValue("session_id")); err != nil {
		writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"channel": "wb", "session_id": r.PathValue("session_id"), "status": "cancelled"}})
}
