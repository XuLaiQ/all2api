package httptransport

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XuLaiQ/all2api/internal/adapters/fake"
	"github.com/XuLaiQ/all2api/internal/config"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestWorkBuddyTokenImportEndpointIsIdempotent(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/import.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	cfg := config.Config{AdminUsername: "admin", AdminPassword: "correct horse battery staple", SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3, CredentialMasterKey: "import-fixture-key"}
	server, err := NewServer(cfg, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	jar, _ := cookiejar.New(nil)
	client := httpServer.Client()
	client.Jar = jar
	login, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/api/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	login.Header.Set("Origin", httpServer.URL)
	resp, err := client.Do(login)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login = %v/%v", err, resp)
	}
	resp.Body.Close()
	body := `{"flow":"token-import","idempotency_key":"import-1","payload":{"accounts":[{"uid":"u1","access_token":"access-1","name":"Demo"}]}}`
	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/api/channels/wb/accounts/provision/import", strings.NewReader(body))
	request.Header.Set("Origin", httpServer.URL)
	request.Header.Set("Content-Type", "application/json")
	first, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("first import = %d %s", first.StatusCode, firstBody)
	}
	request, _ = http.NewRequest(http.MethodPost, httpServer.URL+"/admin/api/channels/wb/accounts/provision/import", strings.NewReader(body))
	request.Header.Set("Origin", httpServer.URL)
	request.Header.Set("Content-Type", "application/json")
	second, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != 200 {
		t.Fatalf("second import = %d %s", second.StatusCode, secondBody)
	}
	var firstPayload, secondPayload struct {
		Data struct {
			Added   int `json:"added"`
			Skipped int `json:"skipped"`
		} `json:"data"`
	}
	if json.Unmarshal(firstBody, &firstPayload) != nil || json.Unmarshal(secondBody, &secondPayload) != nil {
		t.Fatal("invalid import response")
	}
	if firstPayload.Data.Added != 1 || secondPayload.Data.Added != 1 {
		t.Fatalf("idempotent results: %s / %s", firstBody, secondBody)
	}
}

func TestPlaygroundChatPersistsConversationHistory(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, t.TempDir()+"/playground-http.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.SQLDB().Exec(`INSERT INTO accounts(id,channel,native_id,name,kind,status,enabled,created_at,updated_at) VALUES ('wb:cn:test','wb','cn:test','test','token','ready',1,unixepoch(),unixepoch())`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQLDB().Exec(`INSERT INTO credentials(credential_ref,channel,account_id,encrypted_payload,created_at,updated_at) VALUES ('fixture','wb','cn:test','fixture',unixepoch(),unixepoch())`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AdminUsername: "admin", AdminPassword: "correct horse battery staple", SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3, CredentialMasterKey: "playground-key"}
	server, err := NewServerWithAdapters(cfg, store, log.New(io.Discard, "", 0), map[string]ports.Adapter{"wb": &fake.Adapter{Slug: "wb", Reply: "hello from fake", Catalogue: []ports.ModelDescriptor{{UpstreamID: "model-a", Capabilities: []string{"chat"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	jar, _ := cookiejar.New(nil)
	client := httpServer.Client()
	client.Jar = jar
	login, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/api/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	login.Header.Set("Origin", httpServer.URL)
	resp, err := client.Do(login)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login=%v/%v", err, resp)
	}
	resp.Body.Close()
	body := `{"channel":"wb","model":"model-a","messages":[{"role":"user","content":"hello"}]}`
	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/api/playground/chat", strings.NewReader(body))
	request.Header.Set("Origin", httpServer.URL)
	request.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(responseBody), "hello from fake") {
		t.Fatalf("chat=%d %s", resp.StatusCode, responseBody)
	}
	list, err := client.Get(httpServer.URL + "/admin/api/playground/conversations")
	if err != nil {
		t.Fatal(err)
	}
	listBody, _ := io.ReadAll(list.Body)
	list.Body.Close()
	if list.StatusCode != 200 || !strings.Contains(string(listBody), "conv_") {
		t.Fatalf("list=%d %s", list.StatusCode, listBody)
	}
}
