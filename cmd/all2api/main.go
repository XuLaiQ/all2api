package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/XuLaiQ/all2api/internal/adapters/chatgpt"
	"github.com/XuLaiQ/all2api/internal/adapters/doubao"
	"github.com/XuLaiQ/all2api/internal/adapters/grok"
	"github.com/XuLaiQ/all2api/internal/adapters/workbuddy"
	keyapp "github.com/XuLaiQ/all2api/internal/application/keys"
	"github.com/XuLaiQ/all2api/internal/config"
	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
	"github.com/XuLaiQ/all2api/internal/infrastructure/security"
	"github.com/XuLaiQ/all2api/internal/ports"
	httptransport "github.com/XuLaiQ/all2api/internal/transport/http"
)

func main() {
	logger := log.New(os.Stderr, "all2api ", log.LstdFlags|log.LUTC)
	envFile, err := config.LoadEnvFile()
	if err != nil {
		logger.Fatalf("env file error: %v", err)
	}
	if envFile != "" {
		logger.Printf("loaded environment file path=%s", envFile)
	}
	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("configuration error: %v", err)
	}

	storage, err := persistence.Open(context.Background(), cfg.DBPath)
	if err != nil {
		logger.Fatalf("storage initialization error: %v", err)
	}
	defer storage.Close()

	credentialBox := cryptoinfra.NewFernet(cfg.CredentialMasterKey)
	if err := prepareStorage(context.Background(), cfg, storage, credentialBox, logger); err != nil {
		logger.Fatalf("storage startup error: %v", err)
	}
	workbuddyClient := workbuddy.NewClient(cfg.WBPlatformBase, func(ctx context.Context, accountID string) (map[string]string, error) {
		return storage.ReadCredential(ctx, "wb", accountID, credentialBox)
	})
	workbuddyProvisioner := workbuddy.NewProvisioner(workbuddyClient, storage, credentialBox)
	var doubaoProvisionClient doubao.ProvisionClient = doubao.NewQRClient(cfg.DoubaoPlatformBase)
	if cfg.DoubaoBrowserEnabled {
		doubaoProvisionClient = doubao.NewBrowserClient(cfg.DoubaoBrowserWorkerBase, cfg.DoubaoBrowserWorkerToken, cfg.DoubaoProfileRoot)
	}
	doubaoProvisioner := doubao.NewProvisioner(doubaoProvisionClient, storage, credentialBox)
	chatgptClient := chatgpt.NewClientWithProxy(cfg.ChatGPTWebBase, cfg.ChatGPTProxy, func(ctx context.Context, accountID string) (map[string]string, error) {
		return storage.ReadCredential(ctx, "chatgpt", accountID, credentialBox)
	})
	chatgptOAuth := chatgpt.NewOAuthClient(cfg.ChatGPTPlatformBase, cfg.ChatGPTProxy)
	chatgptProvisioner := chatgpt.NewProvisioner(chatgptOAuth, storage, credentialBox)
	chatgptClient.EnableTokenRefresh(chatgptOAuth, func(ctx context.Context, accountID string, credentials map[string]string) error {
		return storage.UpdateCredential(ctx, "chatgpt", accountID, credentials, credentialBox, ports.AuditEvent{Actor: "system", Action: "refresh_credential"})
	})
	doubaoClient := doubao.NewClient(cfg.DoubaoPlatformBase, func(ctx context.Context, accountID string) (map[string]string, error) {
		return storage.ReadCredential(ctx, "doubao", accountID, credentialBox)
	})
	grokClient := grok.NewClientWithBuildBase(cfg.GrokPlatformBase, cfg.GrokBuildBase, func(ctx context.Context, accountID string) (map[string]string, error) {
		return storage.ReadCredential(ctx, "grok", accountID, credentialBox)
	})
	if err := grokClient.SetProxy(cfg.GrokProxy); err != nil {
		logger.Fatalf("configuration error: %v", err)
	}
	// Build refresh tokens rotate: every successful refresh must be stored.
	grokClient.PersistCredential = func(ctx context.Context, accountID string, credentials map[string]string) error {
		return storage.UpdateCredential(ctx, "grok", accountID, credentials, credentialBox, ports.AuditEvent{Actor: "system", Action: "refresh_credential"})
	}
	grokProvisioner := grok.NewProvisioner(grokClient.OAuth, storage, credentialBox)
	adapters := map[string]ports.Adapter{
		"wb":      workbuddy.NewAdapter(workbuddyClient),
		"chatgpt": chatgpt.NewAdapter(chatgptClient),
		"doubao":  doubao.NewAdapter(doubaoClient),
		"grok":    grok.NewAdapter(grokClient),
	}
	api, err := httptransport.NewServerWithAdapters(cfg, storage, logger, adapters, workbuddyProvisioner, doubaoProvisioner, chatgptProvisioner, grokProvisioner)
	if err != nil {
		logger.Fatalf("security initialization error: %v", err)
	}
	server := &http.Server{
		Addr:              httptransport.ListenAddress(cfg),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Printf("listening addr=%s", server.Addr)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("server error: %v", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Printf("graceful shutdown failed: %v", err)
		}
	}
}

// prepareStorage applies configuration-owned state before serving traffic:
// the bootstrap API key follows A2A_BOOTSTRAP_API_KEY, and stale provision
// sessions/idempotency records from previous runs are removed.
func prepareStorage(ctx context.Context, cfg config.Config, storage *persistence.DB, box ports.SecretBox, logger *log.Logger) error {
	keys := keyapp.NewService(storage, box, security.NewKeyMaterial(), nil)
	if err := keys.SyncBootstrap(ctx, storage, cfg.BootstrapAPIKey, cfg.BootstrapRPM); err != nil {
		return err
	}
	removed, err := storage.PurgeExpiredProvisionState(ctx, time.Now())
	if err != nil {
		return err
	}
	if removed > 0 {
		logger.Printf("purged expired provision records count=%d", removed)
	}
	return nil
}
