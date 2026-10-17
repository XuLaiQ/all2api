package httptransport

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	chatgptadapter "github.com/XuLaiQ/all2api/internal/adapters/chatgpt"
	doubaoadapter "github.com/XuLaiQ/all2api/internal/adapters/doubao"
	grokadapter "github.com/XuLaiQ/all2api/internal/adapters/grok"
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
	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	mediainfra "github.com/XuLaiQ/all2api/internal/infrastructure/media"
	"github.com/XuLaiQ/all2api/internal/infrastructure/security"
	"github.com/XuLaiQ/all2api/internal/ports"
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
	grokProvision      *grokadapter.Provisioner
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
			func() []string { return []string{"wb", "doubao", "chatgpt", "grok"} },
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
		case *grokadapter.Provisioner:
			server.grokProvision = value
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

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request, _ bool) (authapp.User, bool) {
	user, err := s.auth.Require(r.Header.Get("Authorization"), requestCookie(r))
	if err != nil {
		writeAdminAuthError(w, r, err)
		return authapp.User{}, false
	}
	return user, true
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

func (s *Server) clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	return security.ClientIP(peer, r.Header.Get("X-Forwarded-For"), s.config.TrustProxy, s.config.TrustedProxies)
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
