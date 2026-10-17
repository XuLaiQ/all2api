package httptransport

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/XuLaiQ/all2api/internal/config"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
)

// routeContractPath is the frozen public route list (originally exported from
// the FastAPI OpenAPI document). Every path in it must stay routed.
const routeContractPath = "../../../docs/migration-baseline/routes.json"

var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

func TestEveryContractRouteIsRegistered(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(routeContractPath))
	if err != nil {
		t.Fatalf("read route contract: %v", err)
	}
	var routes []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(raw, &routes); err != nil {
		t.Fatalf("parse route contract: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("route contract is empty")
	}

	store, err := persistence.Open(context.Background(), t.TempDir()+"/routes.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewServer(config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3,
	}, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	// Guard against a vacuous pass: unknown paths must report no pattern.
	if _, pattern := server.mux.Handler(httptest.NewRequest(http.MethodGet, "/admin/api/no-such-route", nil)); pattern != "" {
		t.Fatalf("unknown path matched pattern %q; coverage check would be meaningless", pattern)
	}
	for _, route := range routes {
		// A concrete value per {param}; "x" also satisfies {name...} wildcards.
		path := pathParameter.ReplaceAllString(route.Path, "x")
		request := httptest.NewRequest(route.Method, path, nil)
		if _, pattern := server.mux.Handler(request); pattern == "" {
			t.Errorf("%s %s is not routed by the Go ServeMux", route.Method, route.Path)
		}
	}
	t.Logf("checked %d contract routes", len(routes))
}
