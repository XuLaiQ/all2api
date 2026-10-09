package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/browser"
)

type server struct {
	worker  *browser.Worker
	enabled bool
	token   string
}

func main() {
	logger := log.New(os.Stderr, "doubao-browser-worker ", log.LstdFlags|log.LUTC)
	config := browser.Config{
		PlatformBase:          strings.TrimRight(envString("A2A_DOUBAO_PLATFORM_BASE", "https://www.doubao.com"), "/"),
		LoginPath:             envString("A2A_DOUBAO_BROWSER_LOGIN_PATH", "/"),
		ProfileRoot:           envString("A2A_DOUBAO_PROFILE_ROOT", "./data/doubao/profiles"),
		Executable:            os.Getenv("A2A_DOUBAO_BROWSER_EXECUTABLE"),
		Headless:              envBool("A2A_DOUBAO_BROWSER_HEADLESS", true),
		MaxContexts:           envInt("A2A_DOUBAO_BROWSER_MAX_CONTEXTS", 4),
		OperationTimeout:      envDuration("A2A_DOUBAO_BROWSER_OPERATION_TIMEOUT_SECONDS", 30),
		LaunchTimeout:         envDuration("A2A_DOUBAO_BROWSER_LAUNCH_TIMEOUT_SECONDS", 30),
		SessionTTL:            envDuration("A2A_DOUBAO_BROWSER_SESSION_TTL_SECONDS", 300),
		QRSelector:            envString("A2A_DOUBAO_BROWSER_QR_SELECTOR", `img[src*="qr"], canvas[data-qr]`),
		QRCodeAttribute:       envString("A2A_DOUBAO_BROWSER_QR_CODE_ATTRIBUTE", "data-code"),
		AuthenticatedSelector: envString("A2A_DOUBAO_BROWSER_AUTHENTICATED_SELECTOR", `[data-testid="user-avatar"], [data-authenticated="true"]`),
	}
	service := &server{
		worker:  browser.New(config),
		enabled: envBool("A2A_DOUBAO_BROWSER_ENABLED", false),
		token:   os.Getenv("A2A_DOUBAO_BROWSER_WORKER_TOKEN"),
	}
	port := envInt("A2A_DOUBAO_BROWSER_WORKER_PORT", 8891)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", service.health)
	mux.HandleFunc("/v1/sessions", service.sessions)
	mux.HandleFunc("/v1/sessions/", service.session)
	address := ":" + strconv.Itoa(port)
	logger.Printf("listening addr=%s enabled=%t", address, service.enabled)
	if err := http.ListenAndServe(address, mux); err != nil {
		logger.Fatal(err)
	}
}

func (s *server) health(writer http.ResponseWriter, _ *http.Request) {
	value := s.worker.Health()
	if !s.enabled || s.token == "" {
		value["status"] = "not_configured"
	}
	writeJSON(writer, http.StatusOK, value)
}

func (s *server) sessions(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "request_error", "method not allowed")
		return
	}
	if !s.authorized(writer, request) {
		return
	}
	if !s.enabled {
		writeError(writer, http.StatusNotImplemented, "worker_capability_pending", "browser worker is not enabled")
		return
	}
	var body struct {
		AccountID   string `json:"account_id"`
		ProfilePath string `json:"profile_path"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.AccountID) == "" {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_request", "account_id is required")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), s.worker.Config.LaunchTimeout+s.worker.Config.OperationTimeout)
	defer cancel()
	challenge, err := s.worker.Start(ctx, body.AccountID, body.ProfilePath)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "browser_worker_unavailable", "browser worker could not start a session")
		return
	}
	writeJSON(writer, http.StatusCreated, challenge)
}

func (s *server) session(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(writer, request) {
		return
	}
	identifier := strings.Trim(strings.TrimPrefix(request.URL.Path, "/v1/sessions/"), "/")
	if identifier == "" || strings.Contains(identifier, "/") {
		writeError(writer, http.StatusNotFound, "not_found", "browser session not found")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), s.worker.Config.OperationTimeout)
	defer cancel()
	switch request.Method {
	case http.MethodGet:
		event, err := s.worker.Poll(ctx, identifier)
		if err != nil {
			writeError(writer, http.StatusNotFound, "not_found", "browser session not found")
			return
		}
		writeJSON(writer, http.StatusOK, event)
	case http.MethodPost:
		event, credentials, err := s.worker.Complete(ctx, identifier)
		if err != nil {
			writeError(writer, http.StatusBadGateway, "browser_worker_error", "browser session could not be completed")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"event": event, "credentials": credentials})
	case http.MethodDelete:
		if err := s.worker.Cancel(identifier); err != nil {
			writeError(writer, http.StatusNotFound, "not_found", "browser session not found")
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	default:
		writeError(writer, http.StatusMethodNotAllowed, "request_error", "method not allowed")
	}
}

func (s *server) authorized(writer http.ResponseWriter, request *http.Request) bool {
	if s.token == "" {
		writeError(writer, http.StatusServiceUnavailable, "worker_token_not_configured", "browser worker token is not configured")
		return false
	}
	if request.Header.Get("X-Worker-Token") != s.token {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "valid worker token required")
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < 1 || value > 65535 {
		return fallback
	}
	return value
}

func envDuration(name string, fallbackSeconds int) time.Duration {
	value, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil || value <= 0 {
		return time.Duration(fallbackSeconds) * time.Second
	}
	return time.Duration(value * float64(time.Second))
}
