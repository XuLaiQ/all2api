package httptransport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	authapp "github.com/XuLaiQ/all2api/internal/application/auth"
	"github.com/XuLaiQ/all2api/internal/infrastructure/security"
)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil || len(body.Username) < 1 || len(body.Username) > 128 || len(body.Password) < 1 || len(body.Password) > 1024 {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "request validation failed", nil)
		return
	}
	result, err := s.auth.Login(r.Context(), body.Username, body.Password, s.clientIP(r))
	if err != nil {
		var limited *authapp.RateLimitedError
		switch {
		case errors.Is(err, authapp.ErrLoginNotConfigured):
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", err.Error(), nil)
		case errors.As(err, &limited):
			writeAdminError(w, r, http.StatusTooManyRequests, "rate_limited", limited.Error(), map[string]string{"Retry-After": strconv.Itoa(limited.WaitSeconds)})
		case errors.Is(err, authapp.ErrInvalidCredentials):
			writeAdminError(w, r, http.StatusUnauthorized, "unauthorized", err.Error(), nil)
		default:
			if s.logger != nil {
				s.logger.Printf("login failed request_id=%s error=%v", requestID(r.Context()), err)
			}
			writeAdminError(w, r, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		}
		return
	}
	http.SetCookie(w, s.sessionCookie(r, result.Cookie, result.ExpiresAt, false))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{
		"data": map[string]any{
			"user":       map[string]string{"username": result.User.Username, "role": result.User.Role},
			"expires_at": result.ExpiresAt.Format(time.RFC3339Nano),
		},
	})
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	cookie := requestCookie(r)
	user, err := s.auth.Require(r.Header.Get("Authorization"), cookie)
	if err != nil {
		writeAdminAuthError(w, r, err)
		return
	}
	var expiresAt *string
	if user.ExpiresAt != nil {
		value := user.ExpiresAt.UTC().Format(time.RFC3339Nano)
		expiresAt = &value
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, map[string]any{
		"data": map[string]any{
			"user":       map[string]string{"username": user.Username, "role": user.Role},
			"expires_at": expiresAt,
		},
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	if err := s.auth.Logout(r.Context(), requestCookie(r), s.clientIP(r)); err != nil {
		if s.logger != nil {
			s.logger.Printf("logout audit failed request_id=%s error=%v", requestID(r.Context()), err)
		}
		writeAdminError(w, r, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	http.SetCookie(w, s.sessionCookie(r, "", time.Unix(1, 0), true))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sessionCookie(r *http.Request, value string, expiresAt time.Time, deleteCookie bool) *http.Cookie {
	maxAge := s.config.SessionDays * 86400
	if deleteCookie {
		maxAge = -1
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return &http.Cookie{
		Name:     authapp.SessionCookieName,
		Value:    value,
		Path:     "/admin/api",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure: security.SecureCookie(
			s.config.SecureCookie,
			scheme,
			remoteHost(r.RemoteAddr),
			r.Header.Get("X-Forwarded-Proto"),
			s.config.TrustProxy,
			s.config.TrustedProxies,
		),
		MaxAge:  maxAge,
		Expires: expiresAt.UTC(),
	}
}

func requestCookie(r *http.Request) string {
	cookie, err := r.Cookie(authapp.SessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func writeAdminAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, authapp.ErrManagementTokenMissing), errors.Is(err, authapp.ErrManagementTokenInvalid):
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", err.Error(), nil)
	case errors.Is(err, authapp.ErrManagementTokenRequired), errors.Is(err, authapp.ErrAdminSessionRequired):
		writeAdminError(w, r, http.StatusUnauthorized, "unauthorized", err.Error(), nil)
	default:
		writeAdminError(w, r, http.StatusUnauthorized, "unauthorized", "valid admin session required", nil)
	}
}
