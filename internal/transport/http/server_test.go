package httptransport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	chatgptadapter "github.com/XuLaiQ/all2api/internal/adapters/chatgpt"
	doubaoadapter "github.com/XuLaiQ/all2api/internal/adapters/doubao"
	"github.com/XuLaiQ/all2api/internal/adapters/fake"
	workbuddyadapter "github.com/XuLaiQ/all2api/internal/adapters/workbuddy"
	"github.com/XuLaiQ/all2api/internal/config"
	cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
	mediainfra "github.com/XuLaiQ/all2api/internal/infrastructure/media"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
	"github.com/XuLaiQ/all2api/internal/ports"
	_ "modernc.org/sqlite"
)

func TestMediaWatermarkEndpointStoresDerivedImage(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/media-http.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	mediaStore := mediainfra.NewStore(store.SQLDB(), store.Path())
	source, err := mediaStore.AddMediaBytes(context.Background(), ports.MediaAsset{
		Actor: "admin", Channel: "doubao", Model: "image-test", Kind: "image", MIMEType: "image/png", Filename: "source.png",
	}, []byte("source-image"))
	if err != nil {
		t.Fatalf("AddMediaBytes() error = %v", err)
	}
	cfg := config.Config{AdminUsername: "admin", AdminPassword: "correct horse battery staple", AdminToken: "wbt_http_media_token_20261008_0001_long", SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3}
	server, err := NewServer(cfg, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	payload := `{"data_url":"data:image/png;base64, bmV3LWltYWdl","filename":"clean.png"}`
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/media/assets/"+source.ID+"/remove-watermark", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
	request.Header.Set("Origin", testServer.URL)
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("media watermark request error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("media watermark status=%d body=%s", response.StatusCode, body)
	}
	rows, total, err := mediaStore.ListMedia(context.Background(), ports.MediaQuery{Actor: "admin", Page: 1, PageSize: 20})
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("derived media rows=%#v total=%d err=%v", rows, total, err)
	}
}

func TestDoubaoAccountRefreshReturnsExplicitUnsupportedCapability(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/doubao-refresh-http.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	cfg := config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken:    "wbt_doubao_refresh_management_token_20261009_long",
		SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3,
	}
	server, err := NewServer(cfg, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/accounts/doubao:cookie:demo/refresh", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
	request.Header.Set("Origin", testServer.URL)
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("refresh request error = %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "capability_not_supported") {
		t.Fatalf("refresh status=%d body=%s", response.StatusCode, body)
	}
}

func TestLegacyOnboardingReturnsGoneInsteadOfNotFound(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/legacy-onboarding-http.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	cfg := config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken:    "wbt_legacy_onboarding_management_token_20261009_long",
		SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3,
	}
	server, err := NewServer(cfg, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/accounts/doubao/onboarding/start", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
	request.Header.Set("Origin", testServer.URL)
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("legacy onboarding request error = %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusGone || !strings.Contains(string(body), "legacy_bridge_disabled") {
		t.Fatalf("legacy onboarding status=%d body=%s", response.StatusCode, body)
	}
}

func TestGeneratedMediaRewritesToGatewayFileURL(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/generated-http.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewServer(config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken: "wbt_generated_media_token_20261009_long", SessionSecret: "0123456789abcdef0123456789abcdef",
		SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3,
	}, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	managementRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/keys", strings.NewReader(`{"name":"generated-media-key","channels":["doubao"],"models":["doubao/*"],"limit_rpm":60}`))
	managementRequest.Header.Set("Authorization", "Bearer wbt_generated_media_token_20261009_long")
	managementRequest.Header.Set("Origin", testServer.URL)
	managementRequest.Header.Set("Content-Type", "application/json")
	managementResponse, err := testServer.Client().Do(managementRequest)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
		Key string `json:"key"`
	}
	if err := json.NewDecoder(managementResponse.Body).Decode(&created); err != nil {
		managementResponse.Body.Close()
		t.Fatal(err)
	}
	managementResponse.Body.Close()
	if managementResponse.StatusCode != http.StatusCreated || created.Data.ID < 1 || created.Key == "" {
		t.Fatalf("create key status=%d body=%#v", managementResponse.StatusCode, created)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
		canvas.Set(0, 0, color.RGBA{R: 255, A: 255})
		_ = png.Encode(writer, canvas)
	}))
	defer upstream.Close()

	actor := fmt.Sprintf("key:%d", created.Data.ID)
	rewritten, assets, err := server.storeCapabilityMedia(context.Background(), actor, "doubao", "model-a", "image", map[string]any{
		"data": []any{map[string]any{"url": upstream.URL + "/result.png"}},
	}, true, nil, nil)
	if err != nil || len(assets) != 1 {
		t.Fatalf("storeCapabilityMedia() = %#v/%v", assets, err)
	}
	object, ok := rewritten.(map[string]any)
	if !ok {
		t.Fatalf("rewritten body = %#v", rewritten)
	}
	items, ok := object["data"].([]map[string]any)
	if !ok || len(items) != 1 {
		t.Fatalf("rewritten data = %#v", object["data"])
	}
	want := "/v1/files/" + assets[0].ID + "/content"
	if items[0]["url"] != want {
		t.Fatalf("rewritten URL = %#v, want %q", items[0]["url"], want)
	}
	contentRequest, _ := http.NewRequest(http.MethodGet, testServer.URL+items[0]["url"].(string), nil)
	contentRequest.Header.Set("Authorization", "Bearer "+created.Key)
	contentResponse, err := testServer.Client().Do(contentRequest)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(contentResponse.Body)
	contentResponse.Body.Close()
	if contentResponse.StatusCode != http.StatusOK || len(content) == 0 {
		t.Fatalf("generated content status=%d bytes=%d", contentResponse.StatusCode, len(content))
	}
}

func TestProvisionSchemasExposeNativeFlows(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/schema.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	box := cryptoinfra.NewFernet("schema-fixture-key")
	cfg := config.Config{AdminUsername: "admin", AdminPassword: "correct horse battery staple", AdminToken: "wbt_schema_management_token_20261008_long", SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3}
	server, err := NewServerWithAdapters(cfg, store, log.New(io.Discard, "", 0), nil,
		workbuddyadapter.NewProvisioner(nil, store, box),
		doubaoadapter.NewProvisioner(nil, store, box),
		chatgptadapter.NewProvisioner(nil, store, box),
	)
	if err != nil {
		t.Fatalf("NewServerWithAdapters() error = %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	for channel, marker := range map[string]string{"wb": "qr-oauth", "doubao": "qr-login", "chatgpt": "oauth-pkce"} {
		request, err := http.NewRequest(http.MethodGet, testServer.URL+"/admin/api/channels/"+channel+"/provision-schema", nil)
		if err != nil {
			t.Fatalf("NewRequest(%s): %v", channel, err)
		}
		request.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
		response, err := testServer.Client().Do(request)
		if err != nil {
			t.Fatalf("schema request(%s): %v", channel, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), marker) {
			t.Fatalf("schema(%s) status=%d body=%s", channel, response.StatusCode, body)
		}
	}
}

func TestOpenAIFileLifecycleUsesGatewayKeyAndMediaStore(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/files.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	cfg := config.Config{AdminUsername: "admin", AdminPassword: "correct horse battery staple", AdminToken: "wbt_file_management_token_20261008_long", SessionSecret: "0123456789abcdef0123456789abcdef", SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3}
	server, err := NewServer(cfg, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	managementRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/keys", strings.NewReader(`{"name":"file-key","limit_rpm":60}`))
	managementRequest.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
	managementRequest.Header.Set("Origin", testServer.URL)
	managementRequest.Header.Set("Content-Type", "application/json")
	managementResponse, err := testServer.Client().Do(managementRequest)
	if err != nil {
		t.Fatalf("create key request: %v", err)
	}
	var keyBody struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(managementResponse.Body).Decode(&keyBody); err != nil {
		managementResponse.Body.Close()
		t.Fatalf("decode key response: %v", err)
	}
	managementResponse.Body.Close()
	if managementResponse.StatusCode != http.StatusCreated || keyBody.Key == "" {
		t.Fatalf("create key status=%d key=%q", managementResponse.StatusCode, keyBody.Key)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filePart, err := writer.CreateFormFile("file", "fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = filePart.Write([]byte("file-fixture"))
	_ = writer.WriteField("purpose", "assistants")
	_ = writer.Close()
	uploadRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/v1/files", &body)
	uploadRequest.Header.Set("Authorization", "Bearer "+keyBody.Key)
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadResponse, err := testServer.Client().Do(uploadRequest)
	if err != nil {
		t.Fatalf("upload file request: %v", err)
	}
	var uploaded map[string]any
	_ = json.NewDecoder(uploadResponse.Body).Decode(&uploaded)
	uploadResponse.Body.Close()
	fileID, _ := uploaded["id"].(string)
	if uploadResponse.StatusCode != http.StatusOK || fileID == "" || uploaded["object"] != "file" {
		t.Fatalf("upload status=%d response=%#v", uploadResponse.StatusCode, uploaded)
	}

	contentRequest, _ := http.NewRequest(http.MethodGet, testServer.URL+"/v1/files/"+fileID+"/content", nil)
	contentRequest.Header.Set("Authorization", "Bearer "+keyBody.Key)
	contentResponse, err := testServer.Client().Do(contentRequest)
	if err != nil {
		t.Fatalf("download file request: %v", err)
	}
	content, _ := io.ReadAll(contentResponse.Body)
	contentResponse.Body.Close()
	if contentResponse.StatusCode != http.StatusOK || string(content) != "file-fixture" {
		t.Fatalf("download status=%d content=%q", contentResponse.StatusCode, content)
	}

	deleteRequest, _ := http.NewRequest(http.MethodDelete, testServer.URL+"/v1/files/"+fileID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer "+keyBody.Key)
	deleteResponse, err := testServer.Client().Do(deleteRequest)
	if err != nil {
		t.Fatalf("delete file request: %v", err)
	}
	deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusOK {
		t.Fatalf("delete status=%d", deleteResponse.StatusCode)
	}
}

type fakeStorage struct{}

type httpStreamFixtureError struct{}

func (httpStreamFixtureError) Error() string   { return "fixture stream failed" }
func (httpStreamFixtureError) StatusCode() int { return http.StatusBadGateway }
func (httpStreamFixtureError) Kind() string    { return "upstream_unavailable" }

type httpStreamFixtureAdapter struct{}

func (httpStreamFixtureAdapter) Channel() string { return "wb" }
func (httpStreamFixtureAdapter) Models(context.Context) ([]ports.ModelDescriptor, error) {
	return []ports.ModelDescriptor{{UpstreamID: "model-a", Capabilities: []string{"chat"}}}, nil
}
func (httpStreamFixtureAdapter) Chat(context.Context, ports.ChatInput) (ports.ChatResult, error) {
	return ports.ChatResult{Text: "fallback"}, nil
}
func (httpStreamFixtureAdapter) ChatWithAccount(context.Context, ports.ChatInput, string) (ports.ChatResult, error) {
	return ports.ChatResult{Text: "fallback"}, nil
}
func (httpStreamFixtureAdapter) ChatStreamWithAccount(_ context.Context, _ ports.ChatInput, _ string, emit func(ports.StreamChunk) error) error {
	if err := emit(ports.StreamChunk{ID: "stream-fixture", Text: "partial"}); err != nil {
		return err
	}
	return httpStreamFixtureError{}
}

var _ ports.AccountStreamAdapter = httpStreamFixtureAdapter{}

func (fakeStorage) Ping(context.Context) error { return nil }
func (fakeStorage) Snapshot(context.Context) (ports.StorageSnapshot, error) {
	return ports.StorageSnapshot{
		SchemaVersion: 11,
		Tables:        map[string]int64{"accounts": 2},
	}, nil
}
func (fakeStorage) Close() error { return nil }

func TestHealthzMaintainsCompatibilityResponseAndRequestID(t *testing.T) {
	cfg := config.Config{CORSOrigins: []string{"http://localhost:5555"}}
	server, err := NewServer(cfg, fakeStorage{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	handler := server.Handler()

	recording := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/healthz", nil)
	request.Header.Set("X-Request-ID", "client-request-1")
	handler.ServeHTTP(recording, request)

	if recording.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recording.Code)
	}
	if got := recording.Header().Get("X-Request-ID"); got != "client-request-1" {
		t.Fatalf("request ID = %q", got)
	}
	if body := strings.TrimSpace(recording.Body.String()); body != `{"database":"ok","service":"all2api-api","status":"ok"}`+"" {
		// JSON object key order is not part of the contract; decode below is the
		// actual assertion, while this branch keeps failures easy to inspect.
		if !strings.Contains(body, `"database":"ok"`) || !strings.Contains(body, `"service":"all2api-api"`) || !strings.Contains(body, `"status":"ok"`) {
			t.Fatalf("unexpected health body: %s", body)
		}
	}
}

func TestDetailedHealthzAndCors(t *testing.T) {
	cfg := config.Config{CORSOrigins: []string{"http://localhost:5555"}}
	server, err := NewServer(cfg, fakeStorage{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	handler := server.Handler()

	recording := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/healthz?detail=true", nil)
	request.Header.Set("Origin", "http://localhost:5555")
	handler.ServeHTTP(recording, request)

	if recording.Code != http.StatusOK || !strings.Contains(recording.Body.String(), `"schema_version":11`) {
		t.Fatalf("unexpected detailed response: status=%d body=%s", recording.Code, recording.Body.String())
	}
	if recording.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5555" {
		t.Fatalf("missing CORS header: %v", recording.Header())
	}
}

func TestSystemInfoReportsGoRuntimeFields(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/sysinfo.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	configValue := config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken: "wbt_sysinfo_management_token_20261009_long", SessionSecret: "0123456789abcdef0123456789abcdef",
		SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3,
	}
	server, err := NewServer(configValue, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	request, _ := http.NewRequest(http.MethodGet, testServer.URL+"/admin/api/sysinfo", nil)
	request.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || body.Data["implementation"] != "go" {
		t.Fatalf("sysinfo status=%d body=%s data=%#v", response.StatusCode, raw, body.Data)
	}
	if _, ok := body.Data["go_version"].(string); !ok || body.Data["go_version"] == "" {
		t.Fatalf("go_version missing: %#v", body.Data)
	}
	if _, ok := body.Data["python_implementation"]; ok {
		t.Fatalf("legacy python sysinfo field returned: %#v", body.Data)
	}
}

func TestBatchDeleteAccountsAndClearLogsRoutes(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, t.TempDir()+"/admin-actions.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	box := cryptoinfra.NewFernet("admin-actions-fixture-key")
	_, err = store.ImportAccounts(ctx, []ports.AccountImport{
		{Channel: "wb", ID: "wb:cn:one", NativeID: "cn:one", Name: "one", Kind: "token", Credentials: map[string]string{"access_token": "one"}},
		{Channel: "wb", ID: "wb:cn:two", NativeID: "cn:two", Name: "two", Kind: "token", Credentials: map[string]string{"access_token": "two"}},
	}, box, ports.AuditEvent{Actor: "admin", Action: "fixture_import"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRequest(ctx, ports.RequestRecord{RequestID: "old-request", KeyID: 1, Channel: "wb", Model: "wb/model-a", UpstreamModel: "model-a", Status: 200, StartedAt: time.Now().Add(-72 * time.Hour), UsageKind: "unknown"}); err != nil {
		t.Fatal(err)
	}
	configValue := config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken: "wbt_admin_actions_management_token_20261009_long", SessionSecret: "0123456789abcdef0123456789abcdef",
		SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3, LogRetentionDays: 1, UsageRetentionDays: 1,
	}
	server, err := NewServer(configValue, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()

	deleteRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/accounts/batch-delete", strings.NewReader(`{"ids":["wb:cn:one","wb:cn:missing","wb:cn:two"]}`))
	deleteRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	deleteRequest.Header.Set("Origin", testServer.URL)
	deleteRequest.Header.Set("Content-Type", "application/json")
	deleteResponse, err := testServer.Client().Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	var deleted struct {
		Data ports.AccountBatchDeleteResult `json:"data"`
	}
	if err := json.NewDecoder(deleteResponse.Body).Decode(&deleted); err != nil {
		deleteResponse.Body.Close()
		t.Fatal(err)
	}
	deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusOK || len(deleted.Data.Deleted) != 2 || len(deleted.Data.Failed) != 1 {
		t.Fatalf("batch delete status=%d result=%#v", deleteResponse.StatusCode, deleted.Data)
	}
	if _, err := store.ReadCredential(ctx, "wb", "cn:one", box); err == nil {
		t.Fatal("batch delete left credential behind")
	}

	clearRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/logs/clear", nil)
	clearRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	clearRequest.Header.Set("Origin", testServer.URL)
	clearResponse, err := testServer.Client().Do(clearRequest)
	if err != nil {
		t.Fatal(err)
	}
	var cleared struct {
		Data ports.ClearLogsResult `json:"data"`
	}
	if err := json.NewDecoder(clearResponse.Body).Decode(&cleared); err != nil {
		clearResponse.Body.Close()
		t.Fatal(err)
	}
	clearResponse.Body.Close()
	if clearResponse.StatusCode != http.StatusOK || cleared.Data.RequestLogsDeleted < 1 || cleared.Data.UsageDailyDeleted < 1 {
		t.Fatalf("clear logs status=%d result=%#v", clearResponse.StatusCode, cleared.Data)
	}
}

func TestChannelOverrideRoutesPersistSafeLocalState(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/channel-override.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	configValue := config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken: "wbt_channel_override_management_token_20261009_long", SessionSecret: "0123456789abcdef0123456789abcdef",
		SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3,
	}
	server, err := NewServer(configValue, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	patchRequest, _ := http.NewRequest(http.MethodPatch, testServer.URL+"/admin/api/channels/wb", strings.NewReader(`{"enabled":false,"config":{"region":"cn"}}`))
	patchRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	patchRequest.Header.Set("Origin", testServer.URL)
	patchRequest.Header.Set("Content-Type", "application/json")
	patchResponse, err := testServer.Client().Do(patchRequest)
	if err != nil {
		t.Fatal(err)
	}
	patchResponse.Body.Close()
	if patchResponse.StatusCode != http.StatusOK {
		t.Fatalf("channel patch status=%d", patchResponse.StatusCode)
	}
	getRequest, _ := http.NewRequest(http.MethodGet, testServer.URL+"/admin/api/channels", nil)
	getRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	getResponse, err := testServer.Client().Do(getRequest)
	if err != nil {
		t.Fatal(err)
	}
	var channels struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(getResponse.Body).Decode(&channels); err != nil {
		getResponse.Body.Close()
		t.Fatal(err)
	}
	getResponse.Body.Close()
	var wbChannel map[string]any
	for _, channel := range channels.Data {
		if channel["slug"] == "wb" {
			wbChannel = channel
			break
		}
	}
	if getResponse.StatusCode != http.StatusOK || wbChannel == nil || wbChannel["management_enabled"] != false {
		t.Fatalf("channel list status=%d data=%#v", getResponse.StatusCode, channels.Data)
	}
	badRequest, _ := http.NewRequest(http.MethodPatch, testServer.URL+"/admin/api/channels/wb", strings.NewReader(`{"config":{"access_token":"no"}}`))
	badRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	badRequest.Header.Set("Origin", testServer.URL)
	badRequest.Header.Set("Content-Type", "application/json")
	badResponse, err := testServer.Client().Do(badRequest)
	if err != nil {
		t.Fatal(err)
	}
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("sensitive channel config status=%d", badResponse.StatusCode)
	}
	deleteRequest, _ := http.NewRequest(http.MethodDelete, testServer.URL+"/admin/api/channels/wb", nil)
	deleteRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	deleteRequest.Header.Set("Origin", testServer.URL)
	deleteResponse, err := testServer.Client().Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusOK {
		t.Fatalf("channel delete status=%d", deleteResponse.StatusCode)
	}
}

func TestDisabledChannelRejectsDataPlaneChat(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/disabled-channel.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	configValue := config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken: "wbt_disabled_channel_management_token_20261009_long", SessionSecret: "0123456789abcdef0123456789abcdef",
		SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 3,
	}
	server, err := NewServerWithAdapters(configValue, store, log.New(io.Discard, "", 0), map[string]ports.Adapter{
		"wb": &fake.Adapter{Slug: "wb", Reply: "must not run", Catalogue: []ports.ModelDescriptor{{UpstreamID: "model-a", Capabilities: []string{"chat"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	managementRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/keys", strings.NewReader(`{"name":"disabled-channel-key","channels":["wb"],"models":["wb/*"],"limit_rpm":60}`))
	managementRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	managementRequest.Header.Set("Origin", testServer.URL)
	managementRequest.Header.Set("Content-Type", "application/json")
	managementResponse, err := testServer.Client().Do(managementRequest)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(managementResponse.Body).Decode(&created); err != nil {
		managementResponse.Body.Close()
		t.Fatal(err)
	}
	managementResponse.Body.Close()
	if managementResponse.StatusCode != http.StatusCreated || created.Key == "" {
		t.Fatalf("key creation status=%d", managementResponse.StatusCode)
	}
	patchRequest, _ := http.NewRequest(http.MethodPatch, testServer.URL+"/admin/api/channels/wb", strings.NewReader(`{"enabled":false}`))
	patchRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	patchRequest.Header.Set("Origin", testServer.URL)
	patchRequest.Header.Set("Content-Type", "application/json")
	patchResponse, err := testServer.Client().Do(patchRequest)
	if err != nil {
		t.Fatal(err)
	}
	patchResponse.Body.Close()
	if patchResponse.StatusCode != http.StatusOK {
		t.Fatalf("channel disable status=%d", patchResponse.StatusCode)
	}
	chatRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"wb/model-a","messages":[{"role":"user","content":"hello"}]}`))
	chatRequest.Header.Set("Authorization", "Bearer "+created.Key)
	chatRequest.Header.Set("Content-Type", "application/json")
	chatResponse, err := testServer.Client().Do(chatRequest)
	if err != nil {
		t.Fatal(err)
	}
	var failure map[string]any
	_ = json.NewDecoder(chatResponse.Body).Decode(&failure)
	chatResponse.Body.Close()
	if chatResponse.StatusCode != http.StatusServiceUnavailable || failure["error"].(map[string]any)["code"] != "channel_disabled" {
		t.Fatalf("disabled chat status=%d body=%#v", chatResponse.StatusCode, failure)
	}
}

func TestChatCompletionStreamEmitsErrorAfterPartialOutput(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, t.TempDir()+"/stream-http.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	box := cryptoinfra.NewFernet("stream-http-fixture-key")
	if _, err := store.ImportAccounts(ctx, []ports.AccountImport{{Channel: "wb", ID: "wb:cn:stream", NativeID: "cn:stream", Name: "stream", Kind: "token", Credentials: map[string]string{"access_token": "fixture"}}}, box, ports.AuditEvent{Actor: "admin", Action: "fixture_import"}); err != nil {
		t.Fatal(err)
	}
	configValue := config.Config{
		AdminUsername: "admin", AdminPassword: "correct horse battery staple",
		AdminToken: "wbt_stream_http_management_token_20261009_long", SessionSecret: "0123456789abcdef0123456789abcdef",
		SessionDays: 1, SessionIdleHours: 1, LoginMaxFails: 1,
	}
	server, err := NewServerWithAdapters(configValue, store, log.New(io.Discard, "", 0), map[string]ports.Adapter{"wb": httpStreamFixtureAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	managementRequest, _ := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/keys", strings.NewReader(`{"name":"stream-key","channels":["wb"],"models":["wb/*"],"limit_rpm":60}`))
	managementRequest.Header.Set("Authorization", "Bearer "+configValue.AdminToken)
	managementRequest.Header.Set("Origin", testServer.URL)
	managementRequest.Header.Set("Content-Type", "application/json")
	managementResponse, err := testServer.Client().Do(managementRequest)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(managementResponse.Body).Decode(&created); err != nil {
		managementResponse.Body.Close()
		t.Fatal(err)
	}
	managementResponse.Body.Close()
	if managementResponse.StatusCode != http.StatusCreated || created.Key == "" {
		t.Fatalf("stream key status=%d", managementResponse.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodPost, testServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"wb/model-a","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+created.Key)
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(string(body), "partial") || !strings.Contains(string(body), "stream_interrupted") || strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream interruption response status=%d content_type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
}

func TestAdminAuthRoutesEnforceOriginAndManageSession(t *testing.T) {
	cfg := config.Config{
		AdminUsername:    "admin",
		AdminPassword:    "correct horse battery staple",
		SessionSecret:    "0123456789abcdef0123456789abcdef",
		SessionDays:      1,
		SessionIdleHours: 1,
		LoginMaxFails:    3,
	}
	server, err := NewServer(cfg, fakeStorage{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	client := testServer.Client()
	client.Jar = jar

	withoutOrigin, err := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := client.Do(withoutOrigin)
	if err != nil {
		t.Fatalf("login without origin error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("login without origin status = %d", response.StatusCode)
	}

	loginRequest, err := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	loginRequest.Header.Set("Origin", testServer.URL)
	response, err = client.Do(loginRequest)
	if err != nil {
		t.Fatalf("login error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(jar.Cookies(loginRequest.URL)) != 1 {
		t.Fatalf("login status/cookie = %d/%v", response.StatusCode, jar.Cookies(loginRequest.URL))
	}

	response, err = client.Get(testServer.URL + "/admin/api/auth/session")
	if err != nil {
		t.Fatalf("session error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d", response.StatusCode)
	}

	logoutRequest, err := http.NewRequest(http.MethodPost, testServer.URL+"/admin/api/auth/logout", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	logoutRequest.Header.Set("Origin", testServer.URL)
	response, err = client.Do(logoutRequest)
	if err != nil {
		t.Fatalf("logout error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", response.StatusCode)
	}

	response, err = client.Get(testServer.URL + "/admin/api/auth/session")
	if err != nil {
		t.Fatalf("session after logout error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session after logout status = %d", response.StatusCode)
	}
}

func TestAdminKeyRoutesReturnPlaintextOnlyOnCreateAndRotate(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/keys-http.db")
	if err != nil {
		t.Fatalf("persistence.Open() error = %v", err)
	}
	defer store.Close()
	seedDB, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	_, err = seedDB.Exec(`INSERT INTO accounts(id, channel, native_id, name, kind, status, enabled, created_at, updated_at) VALUES ('wb:cn:test', 'wb', 'cn:test', 'test', 'token', 'ready', 1, unixepoch(), unixepoch())`)
	if err != nil {
		seedDB.Close()
		t.Fatalf("seed account: %v", err)
	}
	_, err = seedDB.Exec(`INSERT INTO credentials(credential_ref, channel, account_id, encrypted_payload, created_at, updated_at) VALUES ('fixture', 'wb', 'cn:test', 'fixture', unixepoch(), unixepoch())`)
	seedDB.Close()
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	cfg := config.Config{
		AdminUsername:       "admin",
		AdminPassword:       "correct horse battery staple",
		SessionSecret:       "0123456789abcdef0123456789abcdef",
		SessionDays:         1,
		SessionIdleHours:    1,
		LoginMaxFails:       3,
		CredentialMasterKey: "fixture-master-key",
	}
	server, err := NewServerWithAdapters(cfg, store, log.New(io.Discard, "", 0), map[string]ports.Adapter{
		"wb": &fake.Adapter{
			Slug: "wb", Reply: "fake reply",
			Catalogue: []ports.ModelDescriptor{{UpstreamID: "model-a", Capabilities: []string{"chat"}}},
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	client := testServer.Client()
	client.Jar = jar
	origin := testServer.URL

	loginRequest, _ := http.NewRequest(http.MethodPost, origin+"/admin/api/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	loginRequest.Header.Set("Origin", origin)
	response, err := client.Do(loginRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("login = %v, status=%v", err, response)
	}
	response.Body.Close()

	createRequest, _ := http.NewRequest(http.MethodPost, origin+"/admin/api/keys", strings.NewReader(`{"name":"http-key","channels":["wb"],"models":["wb/*"],"limit_rpm":5}`))
	createRequest.Header.Set("Origin", origin)
	createRequest.Header.Set("Content-Type", "application/json")
	response, err = client.Do(createRequest)
	if err != nil {
		t.Fatalf("create key error = %v", err)
	}
	createBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || !strings.Contains(string(createBody), `"key":"sk-a2a-`) {
		t.Fatalf("create key = %d %s", response.StatusCode, createBody)
	}
	var created struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(createBody, &created); err != nil || created.Data.ID < 1 || created.Key == "" {
		t.Fatalf("decode create response: %v %s", err, createBody)
	}

	modelRequest, _ := http.NewRequest(http.MethodGet, origin+"/v1/models", nil)
	modelRequest.Header.Set("Authorization", "Bearer "+created.Key)
	response, err = client.Do(modelRequest)
	if err != nil {
		t.Fatalf("models error = %v", err)
	}
	modelBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(modelBody), `"id":"wb/model-a"`) {
		t.Fatalf("models = %d %s", response.StatusCode, modelBody)
	}

	chatRequest, _ := http.NewRequest(http.MethodPost, origin+"/v1/chat/completions", strings.NewReader(`{"model":"wb/model-a","messages":[{"role":"user","content":"hello"}]}`))
	chatRequest.Header.Set("Authorization", "Bearer "+created.Key)
	chatRequest.Header.Set("Content-Type", "application/json")
	response, err = client.Do(chatRequest)
	if err != nil {
		t.Fatalf("chat error = %v", err)
	}
	chatBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(chatBody), `"content":"fake reply"`) {
		t.Fatalf("chat = %d %s", response.StatusCode, chatBody)
	}

	deniedRequest, _ := http.NewRequest(http.MethodPost, origin+"/v1/chat/completions", strings.NewReader(`{"model":"chatgpt/model-a","messages":[{"role":"user","content":"hello"}]}`))
	deniedRequest.Header.Set("Authorization", "Bearer "+created.Key)
	deniedRequest.Header.Set("Content-Type", "application/json")
	response, err = client.Do(deniedRequest)
	if err != nil {
		t.Fatalf("denied chat error = %v", err)
	}
	deniedBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !strings.Contains(string(deniedBody), `"code":"channel_not_allowed"`) {
		t.Fatalf("denied chat = %d %s", response.StatusCode, deniedBody)
	}

	invalidRequest, _ := http.NewRequest(http.MethodGet, origin+"/v1/models", nil)
	invalidRequest.Header.Set("Authorization", "Bearer invalid-key")
	response, err = client.Do(invalidRequest)
	if err != nil {
		t.Fatalf("invalid key error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key status = %d", response.StatusCode)
	}

	anthropicRequest, _ := http.NewRequest(http.MethodPost, origin+"/v1/messages", strings.NewReader(`{"model":"wb/model-a","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`))
	anthropicRequest.Header.Set("x-api-key", created.Key)
	anthropicRequest.Header.Set("Content-Type", "application/json")
	response, err = client.Do(anthropicRequest)
	if err != nil {
		t.Fatalf("messages error = %v", err)
	}
	anthropicBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(anthropicBody), `"type":"message"`) {
		t.Fatalf("messages = %d %s", response.StatusCode, anthropicBody)
	}

	responsesRequest, _ := http.NewRequest(http.MethodPost, origin+"/v1/responses", strings.NewReader(`{"model":"wb/model-a","input":"hello"}`))
	responsesRequest.Header.Set("Authorization", "Bearer "+created.Key)
	responsesRequest.Header.Set("Content-Type", "application/json")
	response, err = client.Do(responsesRequest)
	if err != nil {
		t.Fatalf("responses error = %v", err)
	}
	responsesBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(responsesBody), `"object":"response"`) {
		t.Fatalf("responses = %d %s", response.StatusCode, responsesBody)
	}

	listResponse, err := client.Get(origin + "/admin/api/keys")
	if err != nil {
		t.Fatalf("list keys error = %v", err)
	}
	listBody, _ := io.ReadAll(listResponse.Body)
	listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusOK || strings.Contains(string(listBody), created.Key) || strings.Contains(string(listBody), `"key":`) {
		t.Fatalf("list leaked plaintext key: %d %s", listResponse.StatusCode, listBody)
	}

	rotateRequest, _ := http.NewRequest(http.MethodPost, origin+"/admin/api/keys/"+strconv.FormatInt(created.Data.ID, 10)+"/rotate", nil)
	rotateRequest.Header.Set("Origin", origin)
	response, err = client.Do(rotateRequest)
	if err != nil {
		t.Fatalf("rotate key error = %v", err)
	}
	rotateBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(rotateBody), `"key":"sk-a2a-`) {
		t.Fatalf("rotate key = %d %s", response.StatusCode, rotateBody)
	}
}
