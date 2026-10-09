package config

import "testing"

func TestLoadReadsCoreEnvironmentAndValidatesRetention(t *testing.T) {
	t.Setenv("A2A_PORT", "8999")
	t.Setenv("A2A_DB_PATH", "./tmp/test.db")
	t.Setenv("A2A_LOG_RETENTION_DAYS", "14")
	t.Setenv("A2A_USAGE_RETENTION_DAYS", "30")
	t.Setenv("A2A_CORS_ORIGINS", "https://console.example, http://localhost:5555")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 8999 || cfg.DBPath != "./tmp/test.db" {
		t.Fatalf("unexpected core config: %+v", cfg)
	}
	if len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[0] != "https://console.example" {
		t.Fatalf("unexpected CORS origins: %#v", cfg.CORSOrigins)
	}
	if cfg.DoubaoBrowserMaxContexts != 4 || !cfg.DoubaoBrowserHeadless {
		t.Fatalf("unexpected browser defaults: %+v", cfg)
	}
}

func TestConfigRejectsUsageRetentionShorterThanLogs(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", Port: 8888, DBPath: "test.db", LogRetentionDays: 30, UsageRetentionDays: 29, ProvisionSessionTTL: 1, SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 1, BootstrapRPM: 1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted an invalid retention window")
	}
}

func TestProductionConfigRejectsInsecureDefaults(t *testing.T) {
	cfg := Config{Environment: "production", Host: "127.0.0.1", Port: 8888, DBPath: "test.db", SessionSecret: "dev-only-change-me", AdminUsername: "admin", AdminPassword: "short", LogRetentionDays: 30, UsageRetentionDays: 30, ProvisionSessionTTL: 1, SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 1, BootstrapRPM: 1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted insecure production defaults")
	}
}

func TestProductionConfigAcceptsStrongSecretsAndRejectsLegacyRuntime(t *testing.T) {
	cfg := Config{Environment: "production", Host: "127.0.0.1", Port: 8888, DBPath: "test.db", SessionSecret: "session-secret-012345678901234567890123", CredentialMasterKey: "credential-master-key-012345678901234567890", AdminUsername: "admin", AdminPassword: "correct horse battery staple", LogRetentionDays: 30, UsageRetentionDays: 30, ProvisionSessionTTL: 1, SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 1, BootstrapRPM: 1, DoubaoBrowserMaxContexts: 1, DoubaoBrowserMaxPagesPerContext: 1, DoubaoBrowserOperationTimeout: 1, DoubaoBrowserLaunchTimeout: 1, DoubaoBrowserSessionTTL: 1}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected strong production config: %v", err)
	}
	cfg.LegacyBridgeEnabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted legacy bridge in production")
	}
}

func TestBrowserWorkerRequiresTokenWhenEnabled(t *testing.T) {
	cfg := Config{Environment: "development", Host: "127.0.0.1", Port: 8888, DBPath: "test.db", DoubaoBrowserEnabled: true, LogRetentionDays: 30, UsageRetentionDays: 30, ProvisionSessionTTL: 1, SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 1, BootstrapRPM: 1, DoubaoBrowserMaxContexts: 1, DoubaoBrowserMaxPagesPerContext: 1, DoubaoBrowserOperationTimeout: 1, DoubaoBrowserLaunchTimeout: 1, DoubaoBrowserSessionTTL: 1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted enabled browser worker without token")
	}
}
