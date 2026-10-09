package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config contains process configuration. Secret fields are intentionally kept
// as plain strings only inside the process; callers must never log this value.
type Config struct {
	Environment string
	Host        string
	Port        int
	BasePath    string

	DBPath    string
	StatePath string

	LogRetentionDays       int
	UsageRetentionDays     int
	ProvisionSessionTTL    int
	SessionDays            int
	SessionIdleHours       int
	LoginMaxFails          int
	BootstrapRPM           int
	AllowWeakAdminPassword bool
	TrustProxy             bool
	SecureCookie           string
	TrustedProxies         []string
	CORSOrigins            []string

	SessionSecret       string
	CredentialMasterKey string
	AdminUsername       string
	AdminPassword       string
	AdminToken          string
	BootstrapAPIKey     string

	WBPlatformBase      string
	DoubaoPlatformBase  string
	ChatGPTPlatformBase string
	ChatGPTWebBase      string
	ChatGPTProxy        string

	DoubaoProfileRoot                  string
	DoubaoBrowserEnabled               bool
	DoubaoBrowserExecutable            string
	DoubaoBrowserHeadless              bool
	DoubaoBrowserLoginPath             string
	DoubaoBrowserMaxContexts           int
	DoubaoBrowserMaxPagesPerContext    int
	DoubaoBrowserOperationTimeout      float64
	DoubaoBrowserLaunchTimeout         float64
	DoubaoBrowserSessionTTL            float64
	DoubaoBrowserQRSelector            string
	DoubaoBrowserQRCodeAttribute       string
	DoubaoBrowserAuthenticatedSelector string
	DoubaoBrowserWorkerBase            string
	DoubaoBrowserWorkerToken           string

	// These fields are migration-only compatibility inputs. They are loaded so
	// the config mapping is complete, but no Go runtime component may consume
	// them as an upstream or source-project dependency.
	LegacyBridgeEnabled       bool
	LegacyWBUpstreamBase      string
	LegacyWBAdminToken        string
	LegacyWBDataKey           string
	LegacyDoubaoUpstreamBase  string
	LegacyDoubaoAPIKey        string
	LegacyChatGPTUpstreamBase string
	LegacyChatGPTAuthKey      string
	WBUpstreamBase            string
	WBAdminToken              string
	WBDataKey                 string
	WBTraceSecret             string
	DoubaoUpstreamBase        string
	DoubaoAPIKey              string
	ChatGPTUpstreamBase       string
	ChatGPTAuthKey            string
}

func Load() (Config, error) {
	cfg := Config{
		Environment:                        "development",
		Host:                               "0.0.0.0",
		Port:                               8888,
		DBPath:                             "./data/all2api.db",
		StatePath:                          "./data/gateway/state.json",
		LogRetentionDays:                   30,
		UsageRetentionDays:                 365,
		ProvisionSessionTTL:                600,
		SessionDays:                        1,
		SessionIdleHours:                   12,
		LoginMaxFails:                      5,
		BootstrapRPM:                       60,
		SessionSecret:                      "dev-only-change-me",
		AdminUsername:                      "admin",
		WBPlatformBase:                     "https://copilot.tencent.com",
		DoubaoPlatformBase:                 "https://www.doubao.com",
		ChatGPTPlatformBase:                "https://auth.openai.com",
		ChatGPTWebBase:                     "https://chatgpt.com",
		SecureCookie:                       "auto",
		TrustProxy:                         true,
		TrustedProxies:                     []string{"127.0.0.1", "::1"},
		CORSOrigins:                        []string{"http://localhost:5555", "http://127.0.0.1:5555"},
		DoubaoProfileRoot:                  "./data/doubao/profiles",
		DoubaoBrowserHeadless:              true,
		DoubaoBrowserLoginPath:             "/",
		DoubaoBrowserMaxContexts:           4,
		DoubaoBrowserMaxPagesPerContext:    2,
		DoubaoBrowserOperationTimeout:      30,
		DoubaoBrowserLaunchTimeout:         30,
		DoubaoBrowserSessionTTL:            300,
		DoubaoBrowserQRSelector:            `img[src*="qr"], canvas[data-qr]`,
		DoubaoBrowserQRCodeAttribute:       "data-code",
		DoubaoBrowserAuthenticatedSelector: `[data-testid="user-avatar"], [data-authenticated="true"]`,
		DoubaoBrowserWorkerBase:            "http://127.0.0.1:8891",
	}

	var err error
	if cfg.Host, err = stringEnv("A2A_HOST", cfg.Host); err != nil {
		return Config{}, err
	}
	if cfg.Port, err = intEnv("A2A_PORT", cfg.Port); err != nil {
		return Config{}, err
	}
	if cfg.BasePath, err = stringEnv("A2A_BASE_PATH", cfg.BasePath); err != nil {
		return Config{}, err
	}
	if cfg.Environment, err = stringEnv("A2A_ENV", cfg.Environment); err != nil {
		return Config{}, err
	}
	if cfg.DBPath, err = stringEnv("A2A_DB_PATH", cfg.DBPath); err != nil {
		return Config{}, err
	}
	if cfg.StatePath, err = stringEnv("A2A_STATE_PATH", cfg.StatePath); err != nil {
		return Config{}, err
	}
	if cfg.LogRetentionDays, err = intEnv("A2A_LOG_RETENTION_DAYS", cfg.LogRetentionDays); err != nil {
		return Config{}, err
	}
	if cfg.UsageRetentionDays, err = intEnv("A2A_USAGE_RETENTION_DAYS", cfg.UsageRetentionDays); err != nil {
		return Config{}, err
	}
	if cfg.ProvisionSessionTTL, err = intEnv("A2A_PROVISION_SESSION_TTL_SECONDS", cfg.ProvisionSessionTTL); err != nil {
		return Config{}, err
	}
	if cfg.SessionDays, err = intEnv("A2A_SESSION_DAYS", cfg.SessionDays); err != nil {
		return Config{}, err
	}
	if cfg.SessionIdleHours, err = intEnv("A2A_SESSION_IDLE_HOURS", cfg.SessionIdleHours); err != nil {
		return Config{}, err
	}
	if cfg.LoginMaxFails, err = intEnv("A2A_LOGIN_MAX_FAILS", cfg.LoginMaxFails); err != nil {
		return Config{}, err
	}
	if cfg.BootstrapRPM, err = intEnv("A2A_BOOTSTRAP_RPM", cfg.BootstrapRPM); err != nil {
		return Config{}, err
	}
	if cfg.AllowWeakAdminPassword, err = boolEnv("A2A_ALLOW_WEAK_ADMIN_PASSWORD", cfg.AllowWeakAdminPassword); err != nil {
		return Config{}, err
	}
	if cfg.TrustProxy, err = boolEnv("A2A_TRUST_PROXY", cfg.TrustProxy); err != nil {
		return Config{}, err
	}
	if cfg.LegacyBridgeEnabled, err = boolEnv("A2A_LEGACY_BRIDGE_ENABLED", cfg.LegacyBridgeEnabled); err != nil {
		return Config{}, err
	}
	if cfg.DoubaoBrowserEnabled, err = boolEnv("A2A_DOUBAO_BROWSER_ENABLED", cfg.DoubaoBrowserEnabled); err != nil {
		return Config{}, err
	}
	if cfg.DoubaoBrowserHeadless, err = boolEnv("A2A_DOUBAO_BROWSER_HEADLESS", cfg.DoubaoBrowserHeadless); err != nil {
		return Config{}, err
	}
	if cfg.DoubaoBrowserMaxContexts, err = intEnv("A2A_DOUBAO_BROWSER_MAX_CONTEXTS", cfg.DoubaoBrowserMaxContexts); err != nil {
		return Config{}, err
	}
	if cfg.DoubaoBrowserMaxPagesPerContext, err = intEnv("A2A_DOUBAO_BROWSER_MAX_PAGES_PER_CONTEXT", cfg.DoubaoBrowserMaxPagesPerContext); err != nil {
		return Config{}, err
	}
	if cfg.DoubaoBrowserOperationTimeout, err = floatEnv("A2A_DOUBAO_BROWSER_OPERATION_TIMEOUT_SECONDS", cfg.DoubaoBrowserOperationTimeout); err != nil {
		return Config{}, err
	}
	if cfg.DoubaoBrowserLaunchTimeout, err = floatEnv("A2A_DOUBAO_BROWSER_LAUNCH_TIMEOUT_SECONDS", cfg.DoubaoBrowserLaunchTimeout); err != nil {
		return Config{}, err
	}
	if cfg.DoubaoBrowserSessionTTL, err = floatEnv("A2A_DOUBAO_BROWSER_SESSION_TTL_SECONDS", cfg.DoubaoBrowserSessionTTL); err != nil {
		return Config{}, err
	}

	for name, target := range map[string]*string{
		"A2A_SESSION_SECRET":                        &cfg.SessionSecret,
		"A2A_CREDENTIAL_MASTER_KEY":                 &cfg.CredentialMasterKey,
		"A2A_ADMIN_USERNAME":                        &cfg.AdminUsername,
		"A2A_ADMIN_PASSWORD":                        &cfg.AdminPassword,
		"A2A_ADMIN_TOKEN":                           &cfg.AdminToken,
		"A2A_BOOTSTRAP_API_KEY":                     &cfg.BootstrapAPIKey,
		"A2A_WB_PLATFORM_BASE":                      &cfg.WBPlatformBase,
		"A2A_DOUBAO_PLATFORM_BASE":                  &cfg.DoubaoPlatformBase,
		"A2A_CHATGPT_PLATFORM_BASE":                 &cfg.ChatGPTPlatformBase,
		"A2A_CHATGPT_WEB_BASE":                      &cfg.ChatGPTWebBase,
		"A2A_CHATGPT_PROXY":                         &cfg.ChatGPTProxy,
		"A2A_SECURE_COOKIE":                         &cfg.SecureCookie,
		"A2A_DOUBAO_PROFILE_ROOT":                   &cfg.DoubaoProfileRoot,
		"A2A_DOUBAO_BROWSER_EXECUTABLE":             &cfg.DoubaoBrowserExecutable,
		"A2A_DOUBAO_BROWSER_LOGIN_PATH":             &cfg.DoubaoBrowserLoginPath,
		"A2A_DOUBAO_BROWSER_QR_SELECTOR":            &cfg.DoubaoBrowserQRSelector,
		"A2A_DOUBAO_BROWSER_QR_CODE_ATTRIBUTE":      &cfg.DoubaoBrowserQRCodeAttribute,
		"A2A_DOUBAO_BROWSER_AUTHENTICATED_SELECTOR": &cfg.DoubaoBrowserAuthenticatedSelector,
		"A2A_DOUBAO_BROWSER_WORKER_BASE":            &cfg.DoubaoBrowserWorkerBase,
		"A2A_DOUBAO_BROWSER_WORKER_TOKEN":           &cfg.DoubaoBrowserWorkerToken,
		"A2A_LEGACY_WB_UPSTREAM_BASE":               &cfg.LegacyWBUpstreamBase,
		"A2A_LEGACY_WB_ADMIN_TOKEN":                 &cfg.LegacyWBAdminToken,
		"A2A_LEGACY_WB_DATA_KEY":                    &cfg.LegacyWBDataKey,
		"A2A_LEGACY_DOUBAO_UPSTREAM_BASE":           &cfg.LegacyDoubaoUpstreamBase,
		"A2A_LEGACY_DOUBAO_API_KEY":                 &cfg.LegacyDoubaoAPIKey,
		"A2A_LEGACY_CHATGPT_UPSTREAM_BASE":          &cfg.LegacyChatGPTUpstreamBase,
		"A2A_LEGACY_CHATGPT_AUTH_KEY":               &cfg.LegacyChatGPTAuthKey,
		"A2A_WB_UPSTREAM_BASE":                      &cfg.WBUpstreamBase,
		"A2A_WB_ADMIN_TOKEN":                        &cfg.WBAdminToken,
		"A2A_WB_DATA_KEY":                           &cfg.WBDataKey,
		"A2A_WB_TRACE_SECRET":                       &cfg.WBTraceSecret,
		"A2A_DOUBAO_UPSTREAM_BASE":                  &cfg.DoubaoUpstreamBase,
		"A2A_DOUBAO_API_KEY":                        &cfg.DoubaoAPIKey,
		"A2A_CHATGPT_UPSTREAM_BASE":                 &cfg.ChatGPTUpstreamBase,
		"A2A_CHATGPT_AUTH_KEY":                      &cfg.ChatGPTAuthKey,
	} {
		if value, ok := os.LookupEnv(name); ok {
			*target = value
		}
	}
	if value, ok := os.LookupEnv("A2A_TRUSTED_PROXIES"); ok {
		cfg.TrustedProxies = splitCSV(value)
	}
	if value, ok := os.LookupEnv("A2A_CORS_ORIGINS"); ok {
		cfg.CORSOrigins = splitCSV(value)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("A2A_HOST must not be empty")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("A2A_PORT must be between 1 and 65535")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		return fmt.Errorf("A2A_DB_PATH must not be empty")
	}
	if c.LogRetentionDays < 1 || c.UsageRetentionDays < 1 {
		return fmt.Errorf("retention days must be positive")
	}
	if c.UsageRetentionDays < c.LogRetentionDays {
		return fmt.Errorf("A2A_USAGE_RETENTION_DAYS must be >= A2A_LOG_RETENTION_DAYS")
	}
	if c.ProvisionSessionTTL < 1 || c.SessionDays < 1 || c.SessionIdleHours < 1 || c.LoginMaxFails < 1 || c.BootstrapRPM < 1 {
		return fmt.Errorf("session, login, and rate-limit settings must be positive")
	}
	if c.DoubaoBrowserMaxContexts < 1 || c.DoubaoBrowserMaxPagesPerContext < 1 || c.DoubaoBrowserOperationTimeout <= 0 || c.DoubaoBrowserLaunchTimeout <= 0 || c.DoubaoBrowserSessionTTL <= 0 {
		return fmt.Errorf("browser limits and timeouts must be positive")
	}
	environment := strings.ToLower(strings.TrimSpace(c.Environment))
	if environment == "" {
		environment = "development"
	}
	if environment != "development" && environment != "test" && environment != "staging" && environment != "production" {
		return fmt.Errorf("A2A_ENV must be development, test, staging, or production")
	}
	if environment == "production" {
		if len(c.SessionSecret) < 32 || c.SessionSecret == "dev-only-change-me" {
			return fmt.Errorf("A2A_SESSION_SECRET must be a non-default value with at least 32 characters in production")
		}
		if len(strings.TrimSpace(c.CredentialMasterKey)) < 32 {
			return fmt.Errorf("A2A_CREDENTIAL_MASTER_KEY must contain at least 32 characters in production")
		}
		if strings.TrimSpace(c.AdminUsername) == "" || len(c.AdminPassword) < 12 {
			return fmt.Errorf("A2A_ADMIN_USERNAME and A2A_ADMIN_PASSWORD must be configured in production")
		}
		if c.AllowWeakAdminPassword {
			return fmt.Errorf("A2A_ALLOW_WEAK_ADMIN_PASSWORD must be false in production")
		}
		if c.LegacyBridgeEnabled || hasLegacyConfiguration(c) {
			return fmt.Errorf("legacy bridge and legacy upstream configuration must be disabled in production")
		}
	}
	if c.DoubaoBrowserEnabled && strings.TrimSpace(c.DoubaoBrowserWorkerToken) == "" {
		return fmt.Errorf("A2A_DOUBAO_BROWSER_WORKER_TOKEN is required when browser worker is enabled")
	}
	return nil
}

func hasLegacyConfiguration(c Config) bool {
	values := []string{
		c.LegacyWBUpstreamBase, c.LegacyWBAdminToken, c.LegacyWBDataKey,
		c.LegacyDoubaoUpstreamBase, c.LegacyDoubaoAPIKey,
		c.LegacyChatGPTUpstreamBase, c.LegacyChatGPTAuthKey,
		c.WBUpstreamBase, c.WBAdminToken, c.WBDataKey, c.WBTraceSecret,
		c.DoubaoUpstreamBase, c.DoubaoAPIKey, c.ChatGPTUpstreamBase, c.ChatGPTAuthKey,
	}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func stringEnv(name, fallback string) (string, error) {
	if value, ok := os.LookupEnv(name); ok {
		return value, nil
	}
	return fallback, nil
}

func intEnv(name string, fallback int) (int, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func floatEnv(name string, fallback float64) (float64, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", name, err)
	}
	return parsed, nil
}

func boolEnv(name string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
