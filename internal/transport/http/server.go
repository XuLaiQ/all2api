package httptransport

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	chatgptadapter "github.com/XuLaiQ/all2api/internal/adapters/chatgpt"
	doubaoadapter "github.com/XuLaiQ/all2api/internal/adapters/doubao"
	"github.com/XuLaiQ/all2api/internal/adapters/registry"
	workbuddyadapter "github.com/XuLaiQ/all2api/internal/adapters/workbuddy"
	accountapp "github.com/XuLaiQ/all2api/internal/application/accounts"
	adminapp "github.com/XuLaiQ/all2api/internal/application/admin"
	authapp "github.com/XuLaiQ/all2api/internal/application/auth"
	gatewayapp "github.com/XuLaiQ/all2api/internal/application/gateway"
	"github.com/XuLaiQ/all2api/internal/application/health"
	keyapp "github.com/XuLaiQ/all2api/internal/application/keys"
	mediaapp "github.com/XuLaiQ/all2api/internal/application/media"
	playgroundapp "github.com/XuLaiQ/all2api/internal/application/playground"
	"github.com/XuLaiQ/all2api/internal/config"
	"github.com/XuLaiQ/all2api/internal/domain/scope"
	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	mediainfra "github.com/XuLaiQ/all2api/internal/infrastructure/media"
	"github.com/XuLaiQ/all2api/internal/infrastructure/security"
	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/XuLaiQ/all2api/internal/protocols"
	"github.com/XuLaiQ/all2api/internal/scheduler"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type Server struct {
	config             config.Config
	health             *health.Service
	auth               *authapp.Service
	keys               *keyapp.Service
	gateway            *gatewayapp.Service
	registry           *registry.Registry
	adminQueries       *adminapp.QueryService
	catalog            *adminapp.CatalogService
	users              *adminapp.UserService
	system             *adminapp.SystemService
	accounts           *accountapp.Service
	provision          *accountapp.ProvisionService
	workbuddyProvision *workbuddyadapter.Provisioner
	doubaoProvision    *doubaoadapter.Provisioner
	chatgptProvision   *chatgptadapter.Provisioner
	media              *mediaapp.Service
	playground         *playgroundapp.Service
	accountSource      ports.AccountSource
	accountPool        *scheduler.Pool
	modelSync          ports.ModelSyncRepository
	auditWriter        ports.AuditWriter
	auditRepository    ports.AuditRepository
	logger             *log.Logger
	mux                *http.ServeMux
}

func NewServer(cfg config.Config, storage ports.Storage, logger *log.Logger) (*Server, error) {
	return NewServerWithAdapters(cfg, storage, logger, nil)
}

func NewServerWithAdapters(cfg config.Config, storage ports.Storage, logger *log.Logger, adapters map[string]ports.Adapter, provisioners ...any) (*Server, error) {
	passwords, err := security.NewPasswordVerifier(cfg.AdminPassword)
	if err != nil {
		return nil, err
	}
	var audit ports.AuditWriter
	if candidate, ok := storage.(ports.AuditWriter); ok {
		audit = candidate
	}
	var keyService *keyapp.Service
	if repository, ok := storage.(ports.KeyRepository); ok {
		keyService = keyapp.NewService(
			repository,
			cryptoinfra.NewFernet(cfg.CredentialMasterKey),
			security.NewKeyMaterial(),
			func() []string { return []string{"wb", "doubao", "chatgpt"} },
		)
	}
	var recorder ports.RequestRecorder
	if candidate, ok := storage.(ports.RequestRecorder); ok {
		recorder = candidate
	}
	var accountPool *scheduler.Pool
	var accountSource ports.AccountSource
	if source, ok := storage.(ports.AccountSource); ok {
		accountSource = source
		accountPool = scheduler.NewPool(source, 1, scheduler.DefaultMaxCandidates)
	}
	var queryRepository ports.AdminQueryRepository
	if candidate, ok := storage.(ports.AdminQueryRepository); ok {
		queryRepository = candidate
	}
	var catalogRepository ports.CatalogRepository
	if candidate, ok := storage.(ports.CatalogRepository); ok {
		catalogRepository = candidate
	}
	var userRepository ports.UserRepository
	if candidate, ok := storage.(ports.UserRepository); ok {
		userRepository = candidate
	}
	var accountRepository ports.AccountRepository
	if candidate, ok := storage.(ports.AccountRepository); ok {
		accountRepository = candidate
	}
	var provisionRepository ports.ProvisionRepository
	if candidate, ok := storage.(ports.ProvisionRepository); ok {
		provisionRepository = candidate
	}
	var mediaRepository ports.MediaRepository
	if candidate, ok := storage.(interface {
		SQLDB() *sql.DB
		Path() string
	}); ok {
		mediaStore := mediainfra.NewStore(candidate.SQLDB(), candidate.Path())
		mediaRepository = mediaStore
	}
	var playgroundRepository ports.PlaygroundRepository
	if candidate, ok := storage.(ports.PlaygroundRepository); ok {
		playgroundRepository = candidate
	}
	var systemRepository ports.SystemRepository
	if candidate, ok := storage.(ports.SystemRepository); ok {
		systemRepository = candidate
	}
	var auditRepository ports.AuditRepository
	if candidate, ok := storage.(ports.AuditRepository); ok {
		auditRepository = candidate
	}
	gatewayService := gatewayapp.NewServiceWithRecorderAndPool(keyService, adapters, recorder, accountPool)
	if channelState, ok := storage.(ports.ChannelStateSource); ok {
		gatewayService.SetChannelState(channelState)
	}
	if routeSource, ok := storage.(ports.RouteSource); ok {
		gatewayService.SetRouteSource(routeSource)
	}
	server := &Server{
		config: cfg,
		health: health.NewService(storage),
		auth: authapp.NewService(
			cfg,
			security.NewSessionManager(cfg.SessionSecret, cfg.SessionDays, cfg.SessionIdleHours),
			security.NewLoginLimiter(cfg.LoginMaxFails),
			passwords,
			audit,
		),
		keys:         keyService,
		gateway:      gatewayService,
		registry:     registry.New(adapters),
		adminQueries: adminapp.NewQueryService(queryRepository),
		catalog:      adminapp.NewCatalogService(catalogRepository),
		users:        adminapp.NewUserService(userRepository),
		system:       adminapp.NewSystemService(systemRepository),
		accounts:     accountapp.NewService(accountRepository),
		provision:    accountapp.NewProvisionService(provisionRepository, cryptoinfra.NewFernet(cfg.CredentialMasterKey)),
		media: mediaapp.NewServiceWithTransformer(mediaRepository, func(data []byte) ([]byte, error) {
			repaired, _, _, err := mediainfra.RemoveWatermark(data)
			return repaired, err
		}),
		playground:    playgroundapp.NewService(playgroundRepository),
		accountSource: accountSource,
		accountPool:   accountPool,
		modelSync: func() ports.ModelSyncRepository {
			if candidate, ok := storage.(ports.ModelSyncRepository); ok {
				return candidate
			}
			return nil
		}(),
		auditWriter:     audit,
		auditRepository: auditRepository,
		logger:          logger,
		mux:             http.NewServeMux(),
	}
	for _, provisioner := range provisioners {
		switch value := provisioner.(type) {
		case *workbuddyadapter.Provisioner:
			server.workbuddyProvision = value
		case *doubaoadapter.Provisioner:
			server.doubaoProvision = value
		case *chatgptadapter.Provisioner:
			server.chatgptProvision = value
		}
	}
	server.mux.HandleFunc("/admin/api/healthz", server.healthz)
	server.mux.HandleFunc("/admin/api/auth/login", server.login)
	server.mux.HandleFunc("/admin/api/auth/session", server.session)
	server.mux.HandleFunc("/admin/api/auth/logout", server.logout)
	server.mux.HandleFunc("/admin/api/keys", server.keysRoot)
	server.mux.HandleFunc("/admin/api/keys/{key_id}", server.keyItem)
	server.mux.HandleFunc("/admin/api/keys/{key_id}/rotate", server.keyRotate)
	server.mux.HandleFunc("/admin/api/channels", server.channels)
	server.mux.HandleFunc("/admin/api/channels/adapters", server.channelAdapters)
	server.mux.HandleFunc("/admin/api/channels/{slug}", server.channels)
	server.mux.HandleFunc("/admin/api/logs", server.logs)
	server.mux.HandleFunc("/admin/api/logs/clear", server.logsClear)
	server.mux.HandleFunc("/admin/api/audit-logs", server.auditLogs)
	server.mux.HandleFunc("/admin/api/stats/summary", server.statsSummary)
	server.mux.HandleFunc("/admin/api/stats/daily", server.statsDaily)
	server.mux.HandleFunc("/admin/api/stats/by-channel", server.statsChannel)
	server.mux.HandleFunc("/admin/api/stats/by-model", server.statsModel)
	server.mux.HandleFunc("/admin/api/stats/by-key", server.statsKey)
	server.mux.HandleFunc("/admin/api/models", server.adminModels)
	server.mux.HandleFunc("/admin/api/models/refresh", server.adminModelsRefresh)
	server.mux.HandleFunc("/admin/api/models/{model_id...}", server.adminModel)
	server.mux.HandleFunc("/admin/api/routes", server.adminRoutes)
	server.mux.HandleFunc("/admin/api/routes/{alias}", server.adminRoute)
	server.mux.HandleFunc("/admin/api/settings", server.adminSettings)
	server.mux.HandleFunc("/admin/api/users", server.usersRoot)
	server.mux.HandleFunc("/admin/api/users/{username}", server.userItem)
	server.mux.HandleFunc("/admin/api/sysinfo", server.sysinfo)
	server.mux.HandleFunc("/admin/api/storage/health", server.storageHealth)
	server.mux.HandleFunc("/admin/api/metrics", server.metrics)
	server.mux.HandleFunc("/admin/api/overview", server.overview)
	server.mux.HandleFunc("/admin/api/accounts", server.accountsRoot)
	server.mux.HandleFunc("/admin/api/accounts/batch-delete", server.accountsBatchDelete)
	server.mux.HandleFunc("/admin/api/accounts/{account_id}/refresh", server.accountItem)
	server.mux.HandleFunc("/admin/api/accounts/{account_id}", server.accountItem)
	server.mux.HandleFunc("/admin/api/accounts/{channel}/onboarding/start", server.legacyOnboarding)
	server.mux.HandleFunc("/admin/api/accounts/{channel}/onboarding/poll", server.legacyOnboarding)
	server.mux.HandleFunc("/admin/api/accounts/{channel}/onboarding/finish", server.legacyOnboarding)
	server.mux.HandleFunc("/admin/api/channels/{channel}/provision-schema", server.provisionSchema)
	server.mux.HandleFunc("/admin/api/channels/{slug}/runtime", server.channelRuntime)
	server.mux.HandleFunc("/admin/api/channels/{slug}/test", server.channelTest)
	server.mux.HandleFunc("/admin/api/channels/{channel}/accounts/provision/import", server.provisionImport)
	server.mux.HandleFunc("/admin/api/channels/{channel}/accounts/provision/start", server.provisionStart)
	server.mux.HandleFunc("/admin/api/channels/{channel}/accounts/provision/{session_id}", server.provisionPoll)
	server.mux.HandleFunc("/admin/api/channels/{channel}/accounts/provision/{session_id}/complete", server.provisionComplete)
	server.mux.HandleFunc("/admin/api/channels/{channel}/accounts/provision/{session_id}/cancel", server.provisionCancel)
	server.mux.HandleFunc("/admin/api/media/assets", server.mediaAssets)
	server.mux.HandleFunc("/admin/api/media/assets/{asset_id}", server.mediaAsset)
	server.mux.HandleFunc("/admin/api/media/assets/{asset_id}/content", server.mediaContent)
	server.mux.HandleFunc("/admin/api/playground/conversations", server.playgroundConversations)
	server.mux.HandleFunc("/admin/api/playground/conversations/{conversation_id}", server.playgroundConversation)
	server.mux.HandleFunc("/admin/api/playground/runs", server.playgroundRuns)
	server.mux.HandleFunc("/admin/api/playground/chat", server.playgroundChat)
	server.mux.HandleFunc("/v1/models", server.models)
	server.mux.HandleFunc("/v1/chat/completions", server.chatCompletions)
	server.mux.HandleFunc("/v1/messages", server.messages)
	server.mux.HandleFunc("/v1/responses", server.responses)
	server.mux.HandleFunc("/v1/files", server.filesRoot)
	server.mux.HandleFunc("/v1/files/{file_id}/content", server.fileContent)
	server.mux.HandleFunc("/v1/files/{file_id}", server.fileItem)
	server.mux.HandleFunc("/v1/images/generations", server.capability)
	server.mux.HandleFunc("/v1/video/generations", server.capability)
	server.mux.HandleFunc("/v1/search", server.capability)
	for _, path := range []string{"/v1/images/edits", "/v1/audio/generations", "/v1/files/download", "/v1/ppt/generations", "/v1/psd/generations", "/v1/editable-file-tasks", "/v1/messages/count_tokens"} {
		server.mux.HandleFunc(path, server.unsupportedDataCapability)
	}
	server.mux.HandleFunc("/admin/api/playground/search", server.playgroundSearch)
	server.mux.HandleFunc("/admin/api/playground/editable-file", server.playgroundEditableFile)
	server.mux.HandleFunc("/admin/api/playground/generations", server.playgroundGeneration)
	server.mux.HandleFunc("/admin/api/playground/files/{task_id}/{filename...}", server.unsupportedAdminCapability)
	server.mux.HandleFunc("/admin/api/media/assets/{asset_id}/remove-watermark", server.mediaWatermark)
	return server, nil
}

func (s *Server) Handler() http.Handler {
	handler := requestIDMiddleware(s.mux)
	handler = s.sameOriginMiddleware(handler)
	return corsMiddleware(s.config.CORSOrigins, handler)
}

func (s *Server) sameOriginMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protectedWrite := strings.HasPrefix(r.URL.Path, "/admin/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		_, hasSession := r.Cookie(authapp.SessionCookieName)
		isLogin := r.URL.Path == "/admin/api/auth/login"
		if protectedWrite && (hasSession == nil || isLogin) && !security.SameOrigin(r.Header.Get("Origin"), r.Host) {
			writeAdminError(w, r, http.StatusForbidden, "forbidden", "origin does not match request host", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	detail, err := parseBoolQuery(r, "detail")
	if err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code":    "validation_error",
				"message": "detail must be a boolean",
			},
		})
		return
	}
	payload, err := s.health.Check(r.Context(), detail)
	if err != nil {
		if s.logger != nil {
			s.logger.Printf("health check failed request_id=%s error=%v", requestID(r.Context()), err)
		}
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{
			"status":   "error",
			"service":  "all2api-api",
			"database": "unavailable",
		})
		return
	}
	writeJSON(w, r, http.StatusOK, payload)
}

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

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	items, err := s.gateway.Models(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	if err != nil {
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, map[string]any{
			"id":       item.Channel + "/" + item.UpstreamID,
			"object":   "model",
			"created":  0,
			"owned_by": item.Channel,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"object": "list", "data": data})
}

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
		total += count
		results = append(results, map[string]any{"channel": channel, "status": "ok", "count": count})
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
		Enabled bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, 422, "validation_error", "enabled must be a boolean", nil)
		return
	}
	item, err := s.catalog.SetModelEnabled(r.Context(), r.PathValue("model_id"), body.Enabled, ports.AuditEvent{Actor: user.Username, Action: "set_model_enabled", IP: s.clientIP(r)})
	if errors.Is(err, ports.ErrKeyNotFound) {
		writeAdminError(w, r, 404, "not_found", "model not found", nil)
		return
	}
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
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

func (s *Server) legacyOnboarding(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r, r.Method != http.MethodGet); !ok {
		return
	}
	writeAdminError(w, r, http.StatusGone, "legacy_bridge_disabled", "legacy onboarding is disabled; use the native channel provision API", nil)
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
	if channel != "wb" && channel != "doubao" && channel != "chatgpt" {
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
	flows := []any{map[string]any{"id": "token-import", "kind": "token_import", "schema": map[string]any{"type": "object", "properties": properties, "required": []string{}, "additionalProperties": true}, "supports": map[string]bool{"import": true}, "timeout_seconds": 300}}
	if channel == "wb" && s.workbuddyProvision != nil {
		flows = append(flows, map[string]any{"id": "qr-oauth", "kind": "oauth", "schema": map[string]any{"type": "object", "properties": map[string]any{"realm": map[string]any{"type": "string", "enum": []string{"cn", "global"}, "default": "cn"}}, "additionalProperties": false}, "supports": map[string]bool{"start": true, "poll": true, "complete": true, "cancel": true}, "timeout_seconds": 300})
	}
	if channel == "doubao" && s.doubaoProvision != nil {
		flows = append(flows, map[string]any{"id": "qr-login", "kind": "qr", "schema": map[string]any{"type": "object", "properties": map[string]any{"account_id": map[string]any{"type": "string"}}, "additionalProperties": false}, "supports": map[string]bool{"start": true, "poll": true, "complete": true, "cancel": true}, "timeout_seconds": 600})
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
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"status": "success", "channel": r.PathValue("channel"), "flow": body.Flow, "idempotency_key": body.IdempotencyKey, "added": result.Added, "skipped": result.Skipped, "errors": result.Errors, "accounts": result.Accounts}})
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

func (s *Server) mediaAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	if s.media == nil {
		writeAdminError(w, r, 503, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	page, size, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	rows, total, err := s.media.List(r.Context(), ports.MediaQuery{Page: page, PageSize: size, Actor: user.Username, Kind: r.URL.Query().Get("kind"), Channel: r.URL.Query().Get("channel"), Search: r.URL.Query().Get("search")})
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, asset := range rows {
		payload := map[string]any{"id": asset.ID, "actor": asset.Actor, "run_id": asset.RunID, "conversation_id": asset.ConversationID, "channel": asset.Channel, "model": asset.Model, "kind": asset.Kind, "mime_type": asset.MIMEType, "filename": asset.Filename, "source_url": asset.SourceURL, "size_bytes": asset.SizeBytes, "metadata": asset.Metadata, "created_at": asset.CreatedAt, "updated_at": asset.UpdatedAt, "content_url": "/admin/api/media/assets/" + asset.ID + "/content?inline=1", "storage_status": "remote"}
		if _, ok := s.media.ContentPath(asset); ok {
			payload["storage_status"] = "stored"
		}
		data = append(data, payload)
	}
	writeJSON(w, r, 200, map[string]any{"data": data, "pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size}})
}

func (s *Server) mediaAsset(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if s.media == nil {
		writeAdminError(w, r, 503, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	if r.Method != http.MethodDelete {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if err := s.media.Delete(r.Context(), r.PathValue("asset_id"), user.Username, ports.AuditEvent{Actor: user.Username, Action: "delete_media", IP: s.clientIP(r)}); err != nil {
		if errors.Is(err, ports.ErrKeyNotFound) {
			writeAdminError(w, r, 404, "not_found", "media asset not found", nil)
		} else {
			writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		}
		return
	}
	w.WriteHeader(204)
}

func (s *Server) mediaContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	if s.media == nil {
		writeAdminError(w, r, 503, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	asset, err := s.media.Get(r.Context(), r.PathValue("asset_id"), user.Username)
	if err != nil {
		writeAdminError(w, r, 404, "not_found", "media asset not found", nil)
		return
	}
	path, ok := s.media.ContentPath(asset)
	if !ok {
		if asset.SourceURL != nil && *asset.SourceURL != "" {
			http.Redirect(w, r, *asset.SourceURL, http.StatusFound)
			return
		}
		writeAdminError(w, r, 404, "not_found", "media content is unavailable", nil)
		return
	}
	if asset.MIMEType != "" {
		w.Header().Set("Content-Type", asset.MIMEType)
	}
	http.ServeFile(w, r, path)
}

func (s *Server) mediaWatermark(w http.ResponseWriter, r *http.Request) {
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
	if s.media == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	source, err := s.media.Get(r.Context(), r.PathValue("asset_id"), user.Username)
	if err != nil || source.Kind != "image" {
		writeAdminError(w, r, http.StatusNotFound, "not_found", "image asset not found", nil)
		return
	}
	var body struct {
		DataURL  string `json:"data_url"`
		Filename string `json:"filename"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 140<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "watermark result is invalid", nil)
		return
	}
	data, mimeType, ok := decodeImageDataURL(body.DataURL)
	if !ok {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "watermark result must be a base64 image", nil)
		return
	}
	filename := strings.TrimSpace(body.Filename)
	if filename == "" {
		filename = source.Filename
		if dot := strings.LastIndex(filename, "."); dot > 0 {
			filename = filename[:dot]
		}
		filename += ".png"
	}
	metadata := map[string]any{"operation": "client_watermark_removal", "derived_from": source.ID}
	asset, err := s.media.AddBytes(r.Context(), ports.MediaAsset{
		Actor:          user.Username,
		RunID:          source.RunID,
		ConversationID: source.ConversationID,
		Channel:        source.Channel,
		Model:          source.Model,
		Kind:           "image",
		MIMEType:       mimeType,
		Filename:       filename,
		Metadata:       metadata,
	}, data)
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "could not store watermark result", nil)
		return
	}
	if s.auditWriter != nil {
		if err := s.auditWriter.RecordAudit(r.Context(), user.Username, "remove_media_watermark", asset.ID, "derived_from="+source.ID, s.clientIP(r)); err != nil {
			writeAdminError(w, r, http.StatusInternalServerError, "internal_error", "could not record media audit", nil)
			return
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": mediaAssetPayload(asset, s.media)})
}

func decodeImageDataURL(value string) ([]byte, string, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "data:image/") {
		return nil, "", false
	}
	comma := strings.IndexByte(value, ',')
	if comma <= len("data:image/") || !strings.Contains(value[:comma], ";base64") {
		return nil, "", false
	}
	header := strings.ToLower(value[:comma])
	mimeType := strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return nil, "", false
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[comma+1:]))
	if err != nil || len(data) == 0 || len(data) > mediainfra.MaxAssetBytes {
		return nil, "", false
	}
	return data, mimeType, true
}

func mediaAssetPayload(asset ports.MediaAsset, service *mediaapp.Service) map[string]any {
	storageStatus := "remote"
	if _, ok := service.ContentPath(asset); ok {
		storageStatus = "stored"
	}
	return map[string]any{
		"id": asset.ID, "actor": asset.Actor, "run_id": asset.RunID, "conversation_id": asset.ConversationID,
		"channel": asset.Channel, "model": asset.Model, "kind": asset.Kind, "mime_type": asset.MIMEType,
		"filename": asset.Filename, "source_url": asset.SourceURL, "size_bytes": asset.SizeBytes,
		"metadata": asset.Metadata, "created_at": asset.CreatedAt, "updated_at": asset.UpdatedAt,
		"content_url": "/admin/api/media/assets/" + asset.ID + "/content?inline=1", "storage_status": storageStatus,
	}
}

func (s *Server) storeCapabilityMedia(ctx context.Context, actor, channel, model, capability string, body any, publicURL bool, runID, conversationID *string) (any, []ports.MediaAsset, error) {
	if s.media == nil {
		return body, nil, nil
	}
	assets, err := s.media.StoreGeneration(ctx, body, mediaapp.GenerationStoreOptions{
		Actor:          actor,
		Channel:        channel,
		Model:          model,
		Capability:     capability,
		RunID:          runID,
		ConversationID: conversationID,
	})
	if err != nil {
		return body, assets, err
	}
	object, ok := body.(map[string]any)
	if !ok || len(assets) == 0 {
		return body, assets, nil
	}
	values := capabilityDataItems(object["data"])
	for fallbackIndex, asset := range assets {
		index := fallbackIndex
		if value, ok := asset.Metadata["generation_index"].(int); ok {
			index = value
		} else if value, ok := asset.Metadata["generation_index"].(float64); ok {
			index = int(value)
		}
		if index < 0 || index >= len(values) {
			continue
		}
		contentURL := "/admin/api/media/assets/" + asset.ID + "/content?inline=1"
		if publicURL {
			contentURL = "/v1/files/" + asset.ID + "/content"
		}
		values[index]["url"] = contentURL
		if capability == "video" {
			values[index]["video_url"] = contentURL
		}
		if free, ok := asset.Metadata["watermark_free"].(bool); ok && free {
			values[index]["watermark_free"] = true
			values[index]["source_variant"] = "go_region_repair"
		}
	}
	object["data"] = values
	return object, assets, nil
}

func capabilityDataItems(value any) []map[string]any {
	switch values := value.(type) {
	case []map[string]any:
		return values
	case []any:
		result := make([]map[string]any, 0, len(values))
		for _, item := range values {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	default:
		return nil
	}
}

func (s *Server) playgroundConversations(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	page, size, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	rows, total, err := s.playground.ListConversations(r.Context(), user.Username, page, size)
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{"data": rows, "pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size}})
}

func (s *Server) playgroundConversation(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	id := r.PathValue("conversation_id")
	switch r.Method {
	case http.MethodGet:
		item, err := s.playground.GetConversation(r.Context(), user.Username, id)
		if err != nil {
			writeAdminError(w, r, 404, "not_found", "conversation not found", nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": item})
	case http.MethodDelete:
		if user.Role != "admin" {
			writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
			return
		}
		deleted, err := s.playground.DeleteConversation(r.Context(), user.Username, id, ports.AuditEvent{Actor: user.Username, Action: "delete_playground_conversation", IP: s.clientIP(r)})
		if err != nil {
			writeAdminError(w, r, 404, "not_found", "conversation not found", nil)
			return
		}
		writeJSON(w, r, 200, map[string]any{"data": map[string]any{"id": id, "deleted": true, "request_records_deleted": deleted}})
	default:
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
	}
}

func (s *Server) playgroundRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	page, size, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	rows, total, err := s.playground.ListRuns(r.Context(), page, size)
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, r, 200, map[string]any{
		"data":       rows,
		"pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size},
	})
}

func (s *Server) unsupportedDataCapability(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, r, http.StatusNotImplemented, "capability_not_supported", "this data-plane capability is not implemented in the Go runtime", nil)
}

func (s *Server) capability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	payload, err := decodeProtocolBody(w, r)
	if err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	capability := "image"
	if strings.Contains(r.URL.Path, "/video/") {
		capability = "video"
	} else if strings.HasSuffix(r.URL.Path, "/search") {
		capability = "search"
	}
	model, _ := payload["model"].(string)
	prompt, _ := payload["prompt"].(string)
	input := ports.CapabilityInput{Capability: capability, Model: model, Prompt: prompt}
	input.Ratio, _ = payload["ratio"].(string)
	input.Size, _ = payload["size"].(string)
	input.Quality, _ = payload["quality"].(string)
	if duration, ok := payload["duration"].(float64); ok {
		value := int(duration)
		input.Duration = &value
	}
	if count, ok := payload["n"].(float64); ok {
		input.Count = int(count)
	}
	if images, ok := payload["base64_images"].([]any); ok {
		for _, image := range images {
			if value, ok := image.(string); ok {
				input.Images = append(input.Images, value)
			}
		}
	}
	if input.Model == "" && capability == "search" {
		input.Model = "chatgpt/auto"
	}
	if input.Model == "" || input.Prompt == "" {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "model and prompt are required", nil)
		return
	}
	key, authErr := s.gateway.Authenticate(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	if authErr != nil {
		s.writeOpenAIServiceError(w, r, authErr)
		return
	}
	channel, _, validModel := strings.Cut(input.Model, "/")
	registration, registered := s.registry.Get(channel)
	if !validModel || !registered || !manifestHasCapability(registration.Manifest.Capabilities, capability) {
		writeOpenAIError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	started := time.Now()
	key, result, err := s.gateway.CapabilityWithKey(r.Context(), key, input)
	if err != nil {
		s.recordGatewayFailure(r, key, input.Model, err, false, started)
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	if result.StatusCode >= 400 {
		s.recordCapabilityRequest(r, key, input.Model, result.StatusCode, "capability_error", started)
		writeJSON(w, r, result.StatusCode, result.Body)
		return
	}
	rewritten, _, storeErr := s.storeCapabilityMedia(r.Context(), fileActor(key), channel, input.Model, capability, result.Body, true, nil, nil)
	if storeErr != nil {
		s.recordCapabilityRequest(r, key, input.Model, http.StatusBadGateway, "media_store_error", started)
		writeOpenAIError(w, r, http.StatusBadGateway, "media_store_error", "generated media could not be stored", nil)
		return
	}
	result.Body = rewritten
	s.recordCapabilityRequest(r, key, input.Model, result.StatusCode, "", started)
	writeJSON(w, r, result.StatusCode, result.Body)
}

func manifestHasCapability(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (s *Server) filesRoot(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticateFileKey(w, r)
	if !ok {
		return
	}
	if s.media == nil {
		writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
		return
	}
	actor := fileActor(key)
	switch r.Method {
	case http.MethodGet:
		page, size, err := parsePagination(r)
		if err != nil {
			writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
			return
		}
		rows, _, err := s.media.List(r.Context(), ports.MediaQuery{Page: page, PageSize: size, Actor: actor, Kind: "other"})
		if err != nil {
			writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
			return
		}
		data := make([]map[string]any, 0, len(rows))
		for _, asset := range rows {
			data = append(data, filePayload(asset))
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"object": "list", "data": data})
	case http.MethodPost:
		if err := s.uploadFile(w, r, key, actor); err != nil {
			status, code, message := http.StatusUnprocessableEntity, "invalid_request", err.Error()
			if errors.Is(err, errFileTooLarge) {
				status, code, message = http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds the 100 MB limit"
			}
			writeOpenAIError(w, r, status, code, message, nil)
		}
	default:
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}

func (s *Server) fileItem(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticateFileKey(w, r)
	if !ok {
		return
	}
	if s.media == nil {
		writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
		return
	}
	id := r.PathValue("file_id")
	if !validFileID(id) {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	asset, err := s.media.Get(r.Context(), id, fileActor(key))
	if errors.Is(err, ports.ErrKeyNotFound) {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	if err != nil {
		writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, r, http.StatusOK, filePayload(asset))
	case http.MethodDelete:
		if err := s.media.Delete(r.Context(), id, fileActor(key), ports.AuditEvent{Actor: fileActor(key), Action: "file.deleted", IP: s.clientIP(r)}); err != nil {
			writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file could not be deleted", nil)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"id": id, "object": "file", "deleted": true})
	default:
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	key, ok := s.authenticateFileKey(w, r)
	if !ok {
		return
	}
	if s.media == nil || !validFileID(r.PathValue("file_id")) {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	asset, err := s.media.Get(r.Context(), r.PathValue("file_id"), fileActor(key))
	if err != nil {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	contentPath, ok := s.media.ContentPath(asset)
	if !ok {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file content is unavailable", nil)
		return
	}
	if asset.MIMEType != "" {
		w.Header().Set("Content-Type", asset.MIMEType)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", asset.Filename))
	http.ServeFile(w, r, contentPath)
}

var errFileTooLarge = errors.New("file is too large")

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request, key ports.APIKey, actor string) error {
	r.Body = http.MaxBytesReader(w, r.Body, mediainfra.MaxAssetBytes+2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			return errFileTooLarge
		}
		return errors.New("multipart file body is invalid")
	}
	fileHeaders := r.MultipartForm.File["file"]
	if len(fileHeaders) == 0 {
		return errors.New("file field is required")
	}
	file, err := fileHeaders[0].Open()
	if err != nil {
		return errors.New("file could not be opened")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, mediainfra.MaxAssetBytes+1))
	if err != nil {
		return errors.New("file could not be read")
	}
	if len(data) == 0 {
		return errors.New("file must not be empty")
	}
	if len(data) > mediainfra.MaxAssetBytes {
		return errFileTooLarge
	}
	mimeType := fileHeaders[0].Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	filename := fileHeaders[0].Filename
	kind := "other"
	purpose := ""
	if values := r.MultipartForm.Value["purpose"]; len(values) > 0 {
		purpose = strings.TrimSpace(values[0])
	}
	if purpose == "" {
		purpose = "assistants"
	}
	asset, err := s.media.AddBytes(r.Context(), ports.MediaAsset{Actor: actor, Channel: "local", Kind: kind, MIMEType: mimeType, Filename: filename, Metadata: map[string]any{"purpose": purpose, "key_id": key.ID}}, data)
	if err != nil {
		return errors.New("file could not be stored")
	}
	if s.auditWriter != nil {
		if err := s.auditWriter.RecordAudit(r.Context(), actor, "file.uploaded", asset.ID, "purpose="+purpose, s.clientIP(r)); err != nil {
			_ = s.media.Delete(r.Context(), asset.ID, actor, ports.AuditEvent{Actor: actor, Action: "file.upload_rollback", IP: s.clientIP(r)})
			return errors.New("file audit could not be recorded")
		}
	}
	writeJSON(w, r, http.StatusOK, filePayload(asset))
	return nil
}

func (s *Server) authenticateFileKey(w http.ResponseWriter, r *http.Request) (ports.APIKey, bool) {
	key, err := s.gateway.Authenticate(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	if err != nil {
		s.writeOpenAIServiceError(w, r, err)
		return ports.APIKey{}, false
	}
	return key, true
}

func fileActor(key ports.APIKey) string { return fmt.Sprintf("key:%d", key.ID) }

func validFileID(value string) bool {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "/\\") {
		return false
	}
	return strings.HasPrefix(value, "asset_")
}

func filePayload(asset ports.MediaAsset) map[string]any {
	purpose := "assistants"
	if value, ok := asset.Metadata["purpose"].(string); ok && value != "" {
		purpose = value
	}
	return map[string]any{"id": asset.ID, "object": "file", "bytes": asset.SizeBytes, "created_at": asset.CreatedAt, "filename": asset.Filename, "purpose": purpose, "status": "processed", "status_details": nil}
}

func (s *Server) unsupportedAdminCapability(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r, r.Method != http.MethodGet); !ok {
		return
	}
	writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this management capability is not implemented in the Go runtime", nil)
}

func (s *Server) playgroundSearch(w http.ResponseWriter, r *http.Request) {
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
	if s.playground == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel        string `json:"channel"`
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		ConversationID string `json:"conversation_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || body.Prompt == "" {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "channel, model and prompt are required", nil)
		return
	}
	registration, registered := s.registry.Get(body.Channel)
	if !registered || !manifestHasCapability(registration.Manifest.Capabilities, "search") {
		writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	conversationID := body.ConversationID
	if conversationID == "" {
		conversationID = fmt.Sprintf("conv_%d", time.Now().UnixNano())
		if err := s.playground.CreateConversation(r.Context(), ports.PlaygroundConversationSummary{ID: conversationID, Actor: user.Username, Title: body.Prompt, Channel: body.Channel, Model: body.Model, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}); err != nil {
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground conversation could not be created", nil)
			return
		}
	}
	runID := fmt.Sprintf("pg_%d", time.Now().UnixNano())
	conversationRef := conversationID
	if err := s.playground.StartRun(r.Context(), ports.PlaygroundRun{ID: runID, ConversationID: &conversationRef, Actor: user.Username, Channel: body.Channel, Model: body.Model, Status: "running", MessageCount: 1, RequestBytes: len(body.Prompt), CreatedAt: time.Now().Unix()}); err != nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground run could not be created", nil)
		return
	}
	result, err := s.gateway.AdminCapability(r.Context(), ports.CapabilityInput{Capability: "search", Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Prompt: body.Prompt})
	if err != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "capability_error")
		if serviceError, ok := err.(*gatewayapp.Error); ok {
			writeAdminError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, nil)
		} else {
			writeAdminError(w, r, http.StatusBadGateway, "capability_error", "playground search failed", nil)
		}
		return
	}
	status := result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	encoded, _ := json.Marshal(result.Body)
	_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: "搜索结果已返回。", Model: body.Model, RawResponse: json.RawMessage(encoded), CreatedAt: time.Now().UnixNano()})
	_ = s.playground.FinishRun(r.Context(), runID, "success", status, "")
	writeJSON(w, r, status, map[string]any{"data": result.Body})
}

func (s *Server) playgroundEditableFile(w http.ResponseWriter, r *http.Request) {
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
	if s.playground == nil || s.media == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel string   `json:"channel"`
		Model   string   `json:"model"`
		Kind    string   `json:"kind"`
		Prompt  string   `json:"prompt"`
		Images  []string `json:"base64_images"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 140<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || body.Prompt == "" || (body.Kind != "ppt" && body.Kind != "psd") {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "channel, model, kind and prompt are required", nil)
		return
	}
	registration, registered := s.registry.Get(body.Channel)
	if !registered || !manifestHasCapability(registration.Manifest.Capabilities, "editable") {
		writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	result, err := s.gateway.AdminCapability(r.Context(), ports.CapabilityInput{Capability: "editable", Kind: body.Kind, Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Prompt: body.Prompt, Images: body.Images})
	if err != nil {
		if serviceError, ok := err.(*gatewayapp.Error); ok {
			writeAdminError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, nil)
		} else {
			writeAdminError(w, r, http.StatusBadGateway, "capability_error", "editable file generation failed", nil)
		}
		return
	}
	bodyMap, ok := result.Body.(map[string]any)
	if !ok {
		writeAdminError(w, r, http.StatusBadGateway, "capability_error", "editable response is invalid", nil)
		return
	}
	rawFiles, _ := bodyMap["files"].([]map[string]any)
	assets := make([]map[string]any, 0, len(rawFiles))
	primaryURL, zipURL := "", ""
	for _, raw := range rawFiles {
		name := stringValueAny(raw["name"])
		content := stringValueAny(raw["content_base64"])
		if name == "" || content == "" {
			continue
		}
		data, decodeErr := base64.StdEncoding.DecodeString(content)
		if decodeErr != nil {
			continue
		}
		mime := stringValueAny(raw["mime_type"])
		if mime == "" {
			mime = "application/octet-stream"
		}
		asset, addErr := s.media.AddBytes(r.Context(), ports.MediaAsset{Actor: user.Username, Channel: body.Channel, Model: body.Model, Kind: body.Kind, MIMEType: mime, Filename: name, Metadata: map[string]any{"prompt": body.Prompt, "task_kind": body.Kind}}, data)
		if addErr != nil {
			continue
		}
		payload := mediaAssetPayload(asset, s.media)
		assets = append(assets, payload)
		contentURL, _ := payload["content_url"].(string)
		if strings.HasSuffix(strings.ToLower(name), ".zip") {
			zipURL = contentURL
		} else if primaryURL == "" {
			primaryURL = contentURL
		}
	}
	if len(assets) == 0 {
		writeAdminError(w, r, http.StatusBadGateway, "capability_error", "editable response contained no downloadable files", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": map[string]any{"task_id": "local_" + fmt.Sprintf("%d", time.Now().UnixNano()), "kind": body.Kind, "status": "success", "primary_url": primaryURL, "zip_url": zipURL, "assets": assets}})
}

func stringValueAny(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func (s *Server) playgroundGeneration(w http.ResponseWriter, r *http.Request) {
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
	if s.playground == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel        string `json:"channel"`
		Model          string `json:"model"`
		Capability     string `json:"capability"`
		Prompt         string `json:"prompt"`
		Ratio          string `json:"ratio"`
		Size           string `json:"size"`
		Quality        string `json:"quality"`
		Duration       *int   `json:"duration"`
		Count          int    `json:"n"`
		ConversationID string `json:"conversation_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || body.Prompt == "" || (body.Capability != "image" && body.Capability != "video") {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "channel, model, capability and prompt are required", nil)
		return
	}
	registration, registered := s.registry.Get(body.Channel)
	if !registered || !manifestHasCapability(registration.Manifest.Capabilities, body.Capability) {
		writeAdminError(w, r, http.StatusNotImplemented, "capability_not_supported", "this capability is not declared by the channel manifest", nil)
		return
	}
	conversationID := body.ConversationID
	if conversationID == "" {
		conversationID = fmt.Sprintf("conv_%d", time.Now().UnixNano())
		if err := s.playground.CreateConversation(r.Context(), ports.PlaygroundConversationSummary{ID: conversationID, Actor: user.Username, Title: body.Prompt, Channel: body.Channel, Model: body.Model, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}); err != nil {
			writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground conversation could not be created", nil)
			return
		}
	}
	runID := fmt.Sprintf("pg_%d", time.Now().UnixNano())
	conversationRef := conversationID
	if err := s.playground.StartRun(r.Context(), ports.PlaygroundRun{ID: runID, ConversationID: &conversationRef, Actor: user.Username, Channel: body.Channel, Model: body.Model, Status: "running", MessageCount: 1, RequestBytes: len(body.Prompt), CreatedAt: time.Now().Unix()}); err != nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "playground run could not be created", nil)
		return
	}
	result, err := s.gateway.AdminCapability(r.Context(), ports.CapabilityInput{Capability: body.Capability, Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Prompt: body.Prompt, Ratio: body.Ratio, Size: body.Size, Quality: body.Quality, Duration: body.Duration, Count: body.Count})
	if err != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "capability_error")
		if serviceError, ok := err.(*gatewayapp.Error); ok {
			writeAdminError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, nil)
		} else {
			writeAdminError(w, r, http.StatusBadGateway, "capability_error", "playground generation failed", nil)
		}
		return
	}
	status := result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	runRef := runID
	conversationRef = conversationID
	rewritten, assets, storeErr := s.storeCapabilityMedia(r.Context(), user.Username, body.Channel, body.Model, body.Capability, result.Body, false, &runRef, &conversationRef)
	if storeErr != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", http.StatusBadGateway, "media_store_error")
		writeAdminError(w, r, http.StatusBadGateway, "media_store_error", "generated media could not be stored", nil)
		return
	}
	result.Body = rewritten
	encoded, _ := json.Marshal(result.Body)
	_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: fmt.Sprintf("已生成 %s 结果。", body.Capability), Model: body.Model, RawResponse: json.RawMessage(encoded), CreatedAt: time.Now().UnixNano()})
	if status >= 400 {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", status, "capability_error")
	} else {
		_ = s.playground.FinishRun(r.Context(), runID, "success", status, "")
	}
	assetPayloads := make([]map[string]any, 0, len(assets))
	for _, asset := range assets {
		assetPayloads = append(assetPayloads, mediaAssetPayload(asset, s.media))
	}
	writeJSON(w, r, status, map[string]any{"data": map[string]any{"run_id": runID, "conversation_id": conversationID, "channel": body.Channel, "model": body.Model, "status": map[bool]string{true: "success", false: "failed"}[status < 400], "response_status": status, "response": result.Body, "assets": assetPayloads}})
}

func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request) {
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
	if s.playground == nil {
		writeAdminError(w, r, 503, "service_unavailable", "playground storage is unavailable", nil)
		return
	}
	var body struct {
		Channel        string        `json:"channel"`
		Model          string        `json:"model"`
		ConversationID string        `json:"conversation_id"`
		Stream         bool          `json:"stream"`
		Messages       []chatMessage `json:"messages"`
		Temperature    *float64      `json:"temperature"`
		MaxTokens      *int          `json:"max_tokens"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Channel == "" || body.Model == "" || len(body.Messages) == 0 {
		writeAdminError(w, r, 422, "validation_error", "channel, model and messages are required", nil)
		return
	}
	conversationID := body.ConversationID
	if conversationID == "" {
		conversationID = fmt.Sprintf("conv_%d", time.Now().UnixNano())
		title := body.Messages[0].Content
		if len(title) > 80 {
			title = title[:80]
		}
		if title == "" {
			title = "新对话"
		}
		if err := s.playground.CreateConversation(r.Context(), ports.PlaygroundConversationSummary{ID: conversationID, Actor: user.Username, Title: title, Channel: body.Channel, Model: body.Model, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}); err != nil {
			writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
			return
		}
	}
	canonical := make([]ports.ChatMessage, 0, len(body.Messages))
	for _, message := range body.Messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			writeAdminError(w, r, 422, "validation_error", "unsupported message role", nil)
			return
		}
		canonical = append(canonical, ports.ChatMessage{Role: message.Role, Content: message.Content})
	}
	last := body.Messages[len(body.Messages)-1]
	if last.Role == "user" {
		_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "user", Content: last.Content, Model: body.Model, CreatedAt: time.Now().UnixNano()})
	}
	runID := fmt.Sprintf("pg_%d", time.Now().UnixNano())
	requestBytes := 0
	encoded, _ := json.Marshal(body)
	requestBytes = len(encoded)
	conversationRef := conversationID
	if err := s.playground.StartRun(r.Context(), ports.PlaygroundRun{ID: runID, ConversationID: &conversationRef, Actor: user.Username, Channel: body.Channel, Model: body.Model, Status: "running", MessageCount: len(body.Messages), RequestBytes: requestBytes, CreatedAt: time.Now().Unix()}); err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	input := ports.ChatInput{Model: body.Channel + "/" + strings.TrimPrefix(body.Model, body.Channel+"/"), Messages: canonical, Temperature: body.Temperature, MaxTokens: body.MaxTokens}
	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, flushOK := w.(http.Flusher)
		if !flushOK {
			_ = s.playground.FinishRun(r.Context(), runID, "failed", 500, "stream_unavailable")
			return
		}
		var answer strings.Builder
		err := s.gateway.AdminChatStream(r.Context(), input, func(chunk ports.StreamChunk) error {
			answer.WriteString(chunk.Text)
			data, _ := json.Marshal(map[string]any{"type": "delta", "content": chunk.Text})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			return nil
		})
		if err != nil {
			_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "adapter_error")
			data, _ := json.Marshal(map[string]any{"type": "error", "message": err.Error(), "response_status": 502, "error_code": "adapter_error"})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			return
		}
		assistant := answer.String()
		_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: assistant, Model: body.Model, CreatedAt: time.Now().UnixNano()})
		_ = s.playground.FinishRun(r.Context(), runID, "success", 200, "")
		done := map[string]any{"type": "done", "run_id": runID, "conversation_id": conversationID, "channel": body.Channel, "model": body.Model, "status": "success", "response_status": 200, "response": map[string]any{"content": assistant}}
		data, _ := json.Marshal(done)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		flusher.Flush()
		return
	}
	result, err := s.gateway.AdminChat(r.Context(), input)
	if err != nil {
		_ = s.playground.FinishRun(r.Context(), runID, "failed", 502, "adapter_error")
		writeAdminError(w, r, 502, "adapter_error", err.Error(), nil)
		return
	}
	response := map[string]any{"content": result.Text, "id": result.ID, "usage": result.Usage}
	_ = s.playground.AddMessage(r.Context(), conversationID, ports.PlaygroundMessage{ID: fmt.Sprintf("msg_%d", time.Now().UnixNano()), Role: "assistant", Content: result.Text, Model: body.Model, RawResponse: response, CreatedAt: time.Now().UnixNano()})
	_ = s.playground.FinishRun(r.Context(), runID, "success", 200, "")
	writeJSON(w, r, 200, map[string]any{"data": map[string]any{"run_id": runID, "conversation_id": conversationID, "channel": body.Channel, "model": body.Model, "status": "success", "response_status": 200, "response": response}})
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	var body chatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "request body is invalid", nil)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object", nil)
		return
	}
	if strings.TrimSpace(body.Model) == "" || len(body.Messages) == 0 {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "model and messages are required", nil)
		return
	}
	messages := make([]ports.ChatMessage, 0, len(body.Messages))
	for _, message := range body.Messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", "unsupported message role", nil)
			return
		}
		messages = append(messages, ports.ChatMessage{Role: message.Role, Content: message.Content})
	}
	started := time.Now()
	chatInput := ports.ChatInput{
		Model: body.Model, Messages: messages, MaxTokens: body.MaxTokens,
		Temperature: body.Temperature, TopP: body.TopP,
	}
	if body.Stream {
		result := ports.ChatResult{ID: "chatcmpl_stream", FinishReason: "stop", CreatedAt: time.Now().Unix()}
		var flusher http.Flusher
		streamStarted := false
		firstChunk := true
		startStream := func() error {
			if streamStarted {
				return nil
			}
			var ok bool
			flusher, ok = beginProtocolStream(w)
			if !ok {
				return fmt.Errorf("streaming is unavailable")
			}
			streamStarted = true
			return nil
		}
		key, streamErr := s.gateway.ChatStream(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), chatInput, func(chunk ports.StreamChunk) error {
			if err := startStream(); err != nil {
				return err
			}
			if result.ID == "chatcmpl_stream" && chunk.ID != "" {
				result.ID = chunk.ID
			}
			if chunk.CreatedAt != 0 {
				result.CreatedAt = chunk.CreatedAt
			}
			if chunk.FinishReason != "" {
				result.FinishReason = chunk.FinishReason
			}
			if chunk.Usage != nil {
				result.Usage = chunk.Usage
			}
			result.UpstreamModel, result.Channel, result.RouteAlias, result.FallbackDepth = chunk.UpstreamModel, chunk.Channel, chunk.RouteAlias, chunk.FallbackDepth
			result.Text += chunk.Text
			delta := map[string]any{"content": chunk.Text}
			if firstChunk {
				delta["role"] = "assistant"
				firstChunk = false
			}
			writeSSE(w, map[string]any{"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": body.Model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}}})
			flusher.Flush()
			return nil
		})
		if streamErr != nil {
			if streamStarted {
				interrupted := &gatewayapp.Error{Status: http.StatusBadGateway, Code: "stream_interrupted", Message: "stream interrupted"}
				s.recordGatewayFailure(r, key, body.Model, interrupted, true, started)
				writeSSE(w, map[string]any{"error": map[string]any{"type": "api_error", "code": "stream_interrupted", "message": "stream interrupted"}})
				flusher.Flush()
			} else {
				s.recordGatewayFailure(r, key, body.Model, streamErr, true, started)
				s.writeOpenAIServiceError(w, r, streamErr)
			}
			return
		}
		if err := startStream(); err != nil {
			writeOpenAIError(w, r, http.StatusInternalServerError, "request_error", err.Error(), nil)
			return
		}
		writeSSE(w, map[string]any{"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": body.Model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": result.FinishReason}}})
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		s.recordGatewaySuccess(r, key, body.Model, result, true, started)
		return
	}
	key, result, err := s.gateway.Chat(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), chatInput)
	if err != nil {
		s.recordGatewayFailure(r, key, body.Model, err, body.Stream, started)
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	response := map[string]any{
		"id":      result.ID,
		"object":  "chat.completion",
		"created": result.CreatedAt,
		"model":   body.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": result.Text},
			"finish_reason": result.FinishReason,
		}},
	}
	if result.Usage != nil {
		response["usage"] = map[string]int{
			"prompt_tokens":     result.Usage.PromptTokens,
			"completion_tokens": result.Usage.CompletionTokens,
			"total_tokens":      result.Usage.PromptTokens + result.Usage.CompletionTokens,
		}
	}
	writeJSON(w, r, http.StatusOK, response)
	s.recordGatewaySuccess(r, key, body.Model, result, false, started)
}

func (s *Server) writeChatStreamChunks(w http.ResponseWriter, model string, chunks []ports.StreamChunk, result ports.ChatResult) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	for _, chunk := range chunks {
		id := chunk.ID
		if id == "" {
			id = result.ID
		}
		created := chunk.CreatedAt
		if created == 0 {
			created = result.CreatedAt
		}
		writeSSE(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": chunk.Text}, "finish_reason": nil}}})
		flusher.Flush()
	}
	writeSSE(w, map[string]any{"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": result.FinishReason}}})
	flusher.Flush()
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed")
		return
	}
	payload, err := decodeProtocolBody(w, r)
	if err != nil {
		writeAnthropicError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input, err := protocols.NormalizeAnthropic(payload)
	if err != nil {
		writeAnthropicError(w, r, http.StatusUnprocessableEntity, "invalid_request", err.Error())
		return
	}
	started := time.Now()
	if input.Stream {
		result := ports.ChatResult{ID: "chatcmpl_stream", FinishReason: "stop", CreatedAt: time.Now().Unix()}
		var flusher http.Flusher
		streamStarted := false
		startStream := func() error {
			if streamStarted {
				return nil
			}
			var ok bool
			flusher, ok = beginProtocolStream(w)
			if !ok {
				return fmt.Errorf("streaming is unavailable")
			}
			writeProtocolEventsNow(w, flusher, protocols.AnthropicStreamStart(result, input.Model))
			streamStarted = true
			return nil
		}
		var textBuilder strings.Builder
		key, streamErr := s.gateway.ChatStream(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input, func(chunk ports.StreamChunk) error {
			if err := startStream(); err != nil {
				return err
			}
			if result.ID == "chatcmpl_stream" && chunk.ID != "" {
				result.ID = chunk.ID
			}
			if chunk.CreatedAt != 0 {
				result.CreatedAt = chunk.CreatedAt
			}
			if chunk.FinishReason != "" {
				result.FinishReason = chunk.FinishReason
			}
			if chunk.Usage != nil {
				result.Usage = chunk.Usage
			}
			result.Channel, result.UpstreamModel, result.RouteAlias, result.FallbackDepth = chunk.Channel, chunk.UpstreamModel, chunk.RouteAlias, chunk.FallbackDepth
			textBuilder.WriteString(chunk.Text)
			writeProtocolEventsNow(w, flusher, [][]byte{protocols.AnthropicStreamDelta(chunk.Text)})
			return nil
		})
		result.Text = textBuilder.String()
		if streamErr != nil {
			s.recordGatewayFailure(r, key, input.Model, streamErr, true, started)
			if streamStarted {
				writeProtocolEventsNow(w, flusher, protocols.AnthropicStreamError(streamErr.Error()))
			} else {
				s.writeAnthropicServiceError(w, r, streamErr)
			}
			return
		}
		if err := startStream(); err != nil {
			writeAnthropicError(w, r, http.StatusInternalServerError, "request_error", err.Error())
			return
		}
		writeProtocolEventsNow(w, flusher, protocols.AnthropicStreamFinish(result, input.Model))
		s.recordGatewaySuccess(r, key, input.Model, result, true, started)
		return
	}
	key, result, err := s.gateway.Chat(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input)
	if err != nil {
		s.recordGatewayFailure(r, key, input.Model, err, input.Stream, started)
		s.writeAnthropicServiceError(w, r, err)
		return
	}
	w.Header().Set("request-id", requestID(r.Context()))
	writeJSON(w, r, http.StatusOK, protocols.AnthropicResponse(result, input.Model))
	s.recordGatewaySuccess(r, key, input.Model, result, false, started)
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	payload, err := decodeProtocolBody(w, r)
	if err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	input, err := protocols.NormalizeResponses(payload)
	if err != nil {
		writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	started := time.Now()
	if input.Stream {
		result := ports.ChatResult{ID: "chatcmpl_stream", FinishReason: "stop", CreatedAt: time.Now().Unix()}
		var flusher http.Flusher
		streamStarted := false
		startStream := func() error {
			if streamStarted {
				return nil
			}
			var ok bool
			flusher, ok = beginProtocolStream(w)
			if !ok {
				return fmt.Errorf("streaming is unavailable")
			}
			writeProtocolEventsNow(w, flusher, protocols.ResponsesStreamStart(result, input.Model))
			streamStarted = true
			return nil
		}
		var textBuilder strings.Builder
		key, streamErr := s.gateway.ChatStream(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input, func(chunk ports.StreamChunk) error {
			if err := startStream(); err != nil {
				return err
			}
			if result.ID == "chatcmpl_stream" && chunk.ID != "" {
				result.ID = chunk.ID
			}
			if chunk.CreatedAt != 0 {
				result.CreatedAt = chunk.CreatedAt
			}
			if chunk.FinishReason != "" {
				result.FinishReason = chunk.FinishReason
			}
			if chunk.Usage != nil {
				result.Usage = chunk.Usage
			}
			result.Channel, result.UpstreamModel, result.RouteAlias, result.FallbackDepth = chunk.Channel, chunk.UpstreamModel, chunk.RouteAlias, chunk.FallbackDepth
			textBuilder.WriteString(chunk.Text)
			writeProtocolEventsNow(w, flusher, [][]byte{protocols.ResponsesStreamDelta(result, chunk.Text)})
			return nil
		})
		result.Text = textBuilder.String()
		if streamErr != nil {
			s.recordGatewayFailure(r, key, input.Model, streamErr, true, started)
			if streamStarted {
				writeProtocolEventsNow(w, flusher, [][]byte{protocols.ResponsesStreamError(streamErr.Error())})
			} else {
				s.writeOpenAIServiceError(w, r, streamErr)
			}
			return
		}
		if err := startStream(); err != nil {
			writeOpenAIError(w, r, http.StatusInternalServerError, "request_error", err.Error(), nil)
			return
		}
		writeProtocolEventsNow(w, flusher, protocols.ResponsesStreamFinish(result, input.Model))
		s.recordGatewaySuccess(r, key, input.Model, result, true, started)
		return
	}
	key, result, err := s.gateway.Chat(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), input)
	if err != nil {
		s.recordGatewayFailure(r, key, input.Model, err, input.Stream, started)
		s.writeOpenAIServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, protocols.ResponsesResponse(result, input.Model))
	s.recordGatewaySuccess(r, key, input.Model, result, false, started)
}

func (s *Server) recordGatewaySuccess(r *http.Request, key ports.APIKey, model string, result ports.ChatResult, stream bool, started time.Time) {
	if key.ID == 0 {
		return
	}
	channel := result.Channel
	if channel == "" {
		channel, _ = splitGatewayModel(model)
	}
	record := ports.RequestRecord{
		RequestID: requestID(r.Context()), KeyID: key.ID,
		Channel: channel, Model: model, UpstreamModel: result.UpstreamModel, RouteAlias: result.RouteAlias, FallbackDepth: result.FallbackDepth, Status: http.StatusOK,
		Stream: stream, UsageKind: "unknown", StartedAt: started,
		LatencyMS: time.Since(started).Milliseconds(),
	}
	if result.Usage != nil {
		record.PromptTokens = result.Usage.PromptTokens
		record.CompletionTokens = result.Usage.CompletionTokens
		record.UsageReported = true
		record.UsageKind = "reported"
	}
	s.recordRequest(r.Context(), record)
}

func (s *Server) recordGatewayFailure(r *http.Request, key ports.APIKey, model string, err error, stream bool, started time.Time) {
	if key.ID == 0 {
		return
	}
	status, code := http.StatusInternalServerError, "api_error"
	var serviceError *gatewayapp.Error
	if errors.As(err, &serviceError) {
		status, code = serviceError.Status, serviceError.Code
	}
	channel, upstream := splitGatewayModel(model)
	routeAlias := ""
	fallbackDepth := 0
	if serviceError != nil {
		if serviceError.Channel != "" {
			channel = serviceError.Channel
		}
		if serviceError.UpstreamModel != "" {
			upstream = serviceError.UpstreamModel
		}
		routeAlias = serviceError.RouteAlias
		fallbackDepth = serviceError.FallbackDepth
	}
	if routeAlias == "" && channel == "" {
		routeAlias = model
	}
	s.recordRequest(r.Context(), ports.RequestRecord{
		RequestID: requestID(r.Context()), KeyID: key.ID,
		Channel: channel, Model: model, UpstreamModel: upstream, RouteAlias: routeAlias, FallbackDepth: fallbackDepth, Status: status,
		ErrorKind: code, Error: code, Stream: stream, UsageKind: "unknown",
		StartedAt: started, LatencyMS: time.Since(started).Milliseconds(),
	})
}

func (s *Server) recordCapabilityRequest(r *http.Request, key ports.APIKey, model string, status int, errorCode string, started time.Time) {
	if key.ID == 0 {
		return
	}
	channel, upstream := splitGatewayModel(model)
	routeAlias := ""
	if channel == "" {
		routeAlias = model
	}
	s.recordRequest(r.Context(), ports.RequestRecord{
		RequestID: requestID(r.Context()), KeyID: key.ID,
		Channel: channel, Model: model, UpstreamModel: upstream, RouteAlias: routeAlias, Status: status,
		ErrorKind: errorCode, Error: errorCode, UsageKind: "unknown",
		StartedAt: started, LatencyMS: time.Since(started).Milliseconds(),
	})
}

func (s *Server) recordRequest(ctx context.Context, record ports.RequestRecord) {
	if err := s.gateway.Record(ctx, record); err != nil && s.logger != nil {
		s.logger.Printf("request record failed request_id=%s error=%v", record.RequestID, err)
	}
}

func splitGatewayModel(model string) (string, string) {
	channel, upstream, ok := strings.Cut(model, "/")
	if !ok {
		return "", model
	}
	return channel, upstream
}

func decodeProtocolBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("request body must contain one JSON object")
	}
	return payload, nil
}

func (s *Server) writeAnthropicServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var serviceError *gatewayapp.Error
	if errors.As(err, &serviceError) {
		w.Header().Set("request-id", requestID(r.Context()))
		writeJSON(w, r, serviceError.Status, protocols.AnthropicError(serviceError.Message, serviceError.Status, serviceError.Code))
		return
	}
	writeAnthropicError(w, r, http.StatusInternalServerError, "api_error", "internal server error")
}

func writeAnthropicError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("request-id", requestID(r.Context()))
	writeJSON(w, r, status, protocols.AnthropicError(message, status, code))
}

func beginProtocolStream(w http.ResponseWriter) (http.Flusher, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	return flusher, true
}

func writeProtocolEventsNow(w http.ResponseWriter, flusher http.Flusher, events [][]byte) {
	for _, event := range events {
		_, _ = w.Write(event)
		flusher.Flush()
	}
}

func (s *Server) writeProtocolEvents(w http.ResponseWriter, events [][]byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	for _, event := range events {
		_, _ = w.Write(event)
		flusher.Flush()
	}
}

func (s *Server) writeChatStream(w http.ResponseWriter, r *http.Request, model string, result ports.ChatResult) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, r, http.StatusInternalServerError, "request_error", "streaming is unavailable", nil)
		return
	}
	writeSSE(w, map[string]any{
		"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": result.Text}, "finish_reason": nil}},
	})
	flusher.Flush()
	writeSSE(w, map[string]any{
		"id": result.ID, "object": "chat.completion.chunk", "created": result.CreatedAt, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": result.FinishReason}},
	})
	flusher.Flush()
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeSSE(w http.ResponseWriter, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
}

func (s *Server) writeOpenAIServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var serviceError *gatewayapp.Error
	if errors.As(err, &serviceError) {
		writeOpenAIError(w, r, serviceError.Status, serviceError.Code, serviceError.Message, serviceError.Headers)
		return
	}
	writeOpenAIError(w, r, http.StatusInternalServerError, "api_error", "internal server error", nil)
}

func writeOpenAIError(w http.ResponseWriter, r *http.Request, status int, code, message string, headers map[string]string) {
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	errorType := "api_error"
	if status == http.StatusUnauthorized {
		errorType = "authentication_error"
	} else if status == http.StatusBadRequest {
		errorType = "invalid_request_error"
	} else if status == http.StatusForbidden {
		errorType = "permission_error"
	}
	writeJSON(w, r, status, map[string]any{
		"error": map[string]any{
			"message": message, "type": errorType, "param": nil, "code": code,
		},
	})
}

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

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request, _ bool) (authapp.User, bool) {
	user, err := s.auth.Require(r.Header.Get("Authorization"), requestCookie(r))
	if err != nil {
		writeAdminAuthError(w, r, err)
		return authapp.User{}, false
	}
	return user, true
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

func parsePagination(r *http.Request) (int, int, error) {
	page, pageSize := 1, 50
	var err error
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 {
			return 0, 0, errors.New("page must be a positive integer")
		}
	}
	if value := r.URL.Query().Get("page_size"); value != "" {
		pageSize, err = strconv.Atoi(value)
		if err != nil || pageSize < 1 || pageSize > 200 {
			return 0, 0, errors.New("page_size must be between 1 and 200")
		}
	}
	return page, pageSize, nil
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

func (s *Server) clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	return security.ClientIP(peer, r.Header.Get("X-Forwarded-For"), s.config.TrustProxy, s.config.TrustedProxies)
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

type requestIDContextKey struct{}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(requestID) {
			requestID = newRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func corsMiddleware(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[origin] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if _, ok := allowed[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
		}
		if r.Method == http.MethodOptions {
			if origin == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if _, ok := allowed[origin]; !ok {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Api-Key, X-Request-ID, Anthropic-Version")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID(r.Context()))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAdminError(w http.ResponseWriter, r *http.Request, status int, code, message string, headers map[string]string) {
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	writeJSON(w, r, status, map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    message,
			"details":    map[string]any{},
			"request_id": requestID(r.Context()),
		},
	})
}

func parseBoolQuery(r *http.Request, name string) (bool, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return false, nil
	}
	return strconv.ParseBool(value)
}

func requestID(ctx context.Context) string {
	if value, ok := ctx.Value(requestIDContextKey{}).(string); ok {
		return value
	}
	return ""
}

func newRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "req_fallback"
	}
	return "req_" + hex.EncodeToString(bytes[:])
}

func ListenAddress(cfg config.Config) string {
	if strings.Contains(cfg.Host, ":") && !strings.HasPrefix(cfg.Host, "[") {
		return "[" + cfg.Host + "]:" + strconv.Itoa(cfg.Port)
	}
	return cfg.Host + ":" + strconv.Itoa(cfg.Port)
}

func remoteHost(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}
