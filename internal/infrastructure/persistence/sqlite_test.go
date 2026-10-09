package persistence

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestOpenMigratesFreshDatabaseAndIsIdempotent(t *testing.T) {
	path := t.TempDir() + "/all2api.db"
	ctx := context.Background()

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", snapshot.SchemaVersion, SchemaVersion)
	}
	if snapshot.Tables["accounts"] != 0 || snapshot.Tables["request_logs"] != 0 {
		t.Fatalf("fresh database has unexpected rows: %#v", snapshot.Tables)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO accounts
		(id, channel, native_id, name, kind, status, created_at, updated_at)
		VALUES ('account-1', 'wb', 'native-1', 'fixture', 'token', 'active', 1, 1)`); err != nil {
		t.Fatalf("insert fixture row: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer store.Close()
	snapshot, err = store.Snapshot(ctx)
	if err != nil {
		t.Fatalf("second Snapshot() error = %v", err)
	}
	if snapshot.Tables["accounts"] != 1 {
		t.Fatalf("existing row was not preserved: %#v", snapshot.Tables)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := t.TempDir() + "/all2api.db"
	ctx := context.Background()
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (?, 1)", SchemaVersion+1); err != nil {
		t.Fatalf("insert future schema version: %v", err)
	}
	_ = store.Close()

	if _, err := Open(ctx, path); err == nil {
		t.Fatal("Open() accepted a newer schema")
	}
}

func TestSyncModelsUpsertsLiveCatalogueWithoutResettingEnablement(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/models.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO models(id, channel, upstream_id, display_name, kind, caps, enabled) VALUES ('wb/model-a', 'wb', 'model-a', 'Old', 'chat', '["chat"]', 0)`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	count, err := store.SyncModels(ctx, "wb", []ports.ModelDescriptor{{UpstreamID: "model-a", DisplayName: "New", Capabilities: []string{"chat"}}, {UpstreamID: "model-b", DisplayName: "Second", Capabilities: []string{"chat"}}}, ports.AuditEvent{Actor: "admin", Action: "refresh_models"})
	if err != nil || count != 2 {
		t.Fatalf("SyncModels() = %d/%v", count, err)
	}
	rows, total, _, err := store.ListModels(ctx, ports.ModelQuery{Channel: "wb", Page: 1, PageSize: 20})
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("ListModels() = %#v/%d/%v", rows, total, err)
	}
	for _, row := range rows {
		if row.ID == "wb/model-a" && row.Enabled {
			t.Fatal("live sync reset an administrator-disabled model")
		}
	}
}

func TestRecordRequestAtomicallyUpdatesLogsAndDailyUsage(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/records.db"
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	started := time.Now().UTC()
	for index := 0; index < 2; index++ {
		if err := store.RecordRequest(ctx, ports.RequestRecord{
			RequestID: "req_" + string(rune('a'+index)), KeyID: 7, Channel: "wb",
			Model: "wb/model-a", UpstreamModel: "model-a", Status: 200,
			PromptTokens: 3, CompletionTokens: 2, UsageReported: true,
			UsageKind: "reported", StartedAt: started, LatencyMS: 10,
		}); err != nil {
			t.Fatalf("RecordRequest() error = %v", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open verification db: %v", err)
	}
	defer db.Close()
	var logs, requests, prompt, completion, reported int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs").Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT requests, prompt_tokens, completion_tokens, usage_reported_requests FROM usage_daily").Scan(&requests, &prompt, &completion, &reported); err != nil {
		t.Fatal(err)
	}
	if logs != 2 || requests != 2 || prompt != 6 || completion != 4 || reported != 2 {
		t.Fatalf("unexpected aggregates: logs=%d requests=%d prompt=%d completion=%d reported=%d", logs, requests, prompt, completion, reported)
	}
	summary, err := store.UsageSummary(ctx, 30)
	if err != nil || summary.Requests != 2 || summary.UsageReportedRequests != 2 {
		t.Fatalf("UsageSummary() = %#v, %v", summary, err)
	}
	rows, err := store.UsageRows(ctx, 30, "key")
	if err != nil || len(rows) != 1 || rows[0].KeyID == nil || *rows[0].KeyID != 7 {
		t.Fatalf("UsageRows() = %#v, %v", rows, err)
	}
}

func TestPlaygroundHistoryPersistsAndDeletesWithAudit(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/playground.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	now := time.Now().Unix()
	summary := ports.PlaygroundConversationSummary{ID: "conv_1", Actor: "admin", Title: "Demo", Channel: "wb", Model: "model-a", CreatedAt: now, UpdatedAt: now}
	if err := store.CreatePlaygroundConversation(ctx, summary); err != nil {
		t.Fatalf("CreatePlaygroundConversation() error = %v", err)
	}
	if err := store.AddPlaygroundMessage(ctx, "conv_1", ports.PlaygroundMessage{ID: "msg_1", Role: "user", Content: "hello", Model: "model-a", CreatedAt: time.Now().UnixNano()}); err != nil {
		t.Fatalf("AddPlaygroundMessage() error = %v", err)
	}
	conversation, err := store.GetPlaygroundConversation(ctx, "admin", "conv_1")
	if err != nil || conversation.MessageCount != 1 {
		t.Fatalf("GetPlaygroundConversation() = %#v/%v", conversation, err)
	}
	conversationID := "conv_1"
	if err := store.CreatePlaygroundRun(ctx, ports.PlaygroundRun{ID: "run_1", ConversationID: &conversationID, Actor: "admin", Channel: "wb", Model: "model-a", Status: "running", MessageCount: 1, CreatedAt: now}); err != nil {
		t.Fatalf("CreatePlaygroundRun() error = %v", err)
	}
	if err := store.FinishPlaygroundRun(ctx, "run_1", "success", 200, "", time.Now()); err != nil {
		t.Fatalf("FinishPlaygroundRun() error = %v", err)
	}
	deleted, err := store.DeletePlaygroundConversation(ctx, "admin", "conv_1", ports.AuditEvent{Actor: "admin"})
	if err != nil || deleted != 1 {
		t.Fatalf("DeletePlaygroundConversation() = %d/%v", deleted, err)
	}
}

func TestRequestLogFiltersAndExtendedFields(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/logs.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ttft := int64(12)
	if err := store.RecordRequest(ctx, ports.RequestRecord{
		RequestID: "req-filter-one", KeyID: 1, Channel: "wb", Model: "wb/model-one", UpstreamModel: "model-one",
		RouteAlias: "priority", FallbackDepth: 2, TTFTMS: &ttft, Status: 502, ErrorKind: "upstream_error", Stream: true,
		StartedAt: time.Now(), UsageKind: "unknown", LatencyMS: 42,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRequest(ctx, ports.RequestRecord{
		RequestID: "req-filter-two", KeyID: 1, Channel: "wb", Model: "wb/other", UpstreamModel: "other",
		Status: 200, ErrorKind: "", Stream: false, StartedAt: time.Now(), UsageKind: "unknown", LatencyMS: 8,
	}); err != nil {
		t.Fatal(err)
	}
	stream := true
	status := 502
	rows, total, err := store.ListRequestLogs(ctx, ports.LogQuery{Page: 1, PageSize: 20, RequestID: "req-filter-one", Model: "model-one", Status: &status, ErrorKind: "upstream_error", Stream: &stream})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("filtered logs = %#v total=%d err=%v", rows, total, err)
	}
	row := rows[0]
	if row.UpstreamModel == nil || *row.UpstreamModel != "model-one" || row.RouteAlias == nil || *row.RouteAlias != "priority" || row.FallbackDepth != 2 || row.TTFTMS == nil || *row.TTFTMS != 12 {
		t.Fatalf("extended log fields = %#v", row)
	}
}
