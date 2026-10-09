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
	"github.com/XuLaiQ/all2api/internal/adapters/workbuddy"
	"github.com/XuLaiQ/all2api/internal/config"
	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
	"github.com/XuLaiQ/all2api/internal/ports"
	httptransport "github.com/XuLaiQ/all2api/internal/transport/http"
)

func main() {
	logger := log.New(os.Stderr, "all2api ", log.LstdFlags|log.LUTC)
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
	chatgptProvisioner := chatgpt.NewProvisioner(chatgpt.NewOAuthClient(cfg.ChatGPTPlatformBase, cfg.ChatGPTProxy), storage, credentialBox)
	doubaoClient := doubao.NewClient(cfg.DoubaoPlatformBase, func(ctx context.Context, accountID string) (map[string]string, error) {
		return storage.ReadCredential(ctx, "doubao", accountID, credentialBox)
	})
	adapters := map[string]ports.Adapter{
		"wb":      workbuddy.NewAdapter(workbuddyClient),
		"chatgpt": chatgpt.NewAdapter(chatgptClient),
		"doubao":  doubao.NewAdapter(doubaoClient),
	}
	api, err := httptransport.NewServerWithAdapters(cfg, storage, logger, adapters, workbuddyProvisioner, doubaoProvisioner, chatgptProvisioner)
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
