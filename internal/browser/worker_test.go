package browser

import (
	"strings"
	"testing"
)

func TestWorkerHealthReportsBrowserConfigurationBoundary(t *testing.T) {
	worker := New(Config{Executable: "C:\\does-not-exist\\chromium.exe", ProfileRoot: t.TempDir()})
	health := worker.Health()
	if health["status"] != "not_configured" || health["runtime"] != "go" {
		t.Fatalf("Health() = %#v", health)
	}
}

func TestWorkerRejectsProfileOutsideConfiguredRoot(t *testing.T) {
	worker := New(Config{ProfileRoot: t.TempDir()})
	if _, err := worker.profilePath("C:\\outside\\profile"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("profilePath() error = %v", err)
	}
}
