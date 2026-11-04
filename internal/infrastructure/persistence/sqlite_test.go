package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
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

func TestClearPlaygroundConversationsRemovesOnlyActorData(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/playground-clear.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	now := time.Now().Unix()
	for _, item := range []ports.PlaygroundConversationSummary{
		{ID: "conv_admin", Actor: "admin", Title: "Admin", Channel: "wb", Model: "m", CreatedAt: now, UpdatedAt: now},
		{ID: "conv_other", Actor: "other", Title: "Other", Channel: "wb", Model: "m", CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.CreatePlaygroundConversation(ctx, item); err != nil {
			t.Fatal(err)
		}
		if err := store.AddPlaygroundMessage(ctx, item.ID, ports.PlaygroundMessage{ID: item.ID + "_msg", Role: "user", Content: "hello", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	adminConversation, otherConversation := "conv_admin", "conv_other"
	for _, run := range []ports.PlaygroundRun{
		{ID: "run_admin", ConversationID: &adminConversation, Actor: "admin", Channel: "wb", Model: "m", Status: "success", CreatedAt: now},
		{ID: "run_other", ConversationID: &otherConversation, Actor: "other", Channel: "wb", Model: "m", Status: "success", CreatedAt: now},
	} {
		if err := store.CreatePlaygroundRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.ClearPlaygroundConversations(ctx, "admin", ports.AuditEvent{Actor: "admin", Action: "clear_playground_conversations"})
	if err != nil || result.ConversationsDeleted != 1 || result.RequestRecordsDeleted != 1 {
		t.Fatalf("ClearPlaygroundConversations() = %#v/%v", result, err)
	}
	if _, err := store.GetPlaygroundConversation(ctx, "admin", "conv_admin"); !errors.Is(err, ports.ErrKeyNotFound) {
		t.Fatalf("admin conversation still exists: %v", err)
	}
	if _, err := store.GetPlaygroundConversation(ctx, "other", "conv_other"); err != nil {
		t.Fatalf("other conversation removed: %v", err)
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
	messages := []ports.PlaygroundMessage{
		{ID: "msg_2", Role: "user", Content: "hello", Model: "model-a", CreatedAt: 100},
		{ID: "msg_1", Role: "assistant", Content: "hi", Model: "model-a", RawResponse: json.RawMessage(`{"answer":"ok","tokens":3}`), CreatedAt: 100},
		{ID: "msg_3", Role: "user", Content: "follow-up", Model: "model-a", CreatedAt: 101},
	}
	for _, message := range messages {
		if err := store.AddPlaygroundMessage(ctx, "conv_1", message); err != nil {
			t.Fatalf("AddPlaygroundMessage(%s) error = %v", message.ID, err)
		}
	}
	conversation, err := store.GetPlaygroundConversation(ctx, "admin", "conv_1")
	if err != nil {
		t.Fatalf("GetPlaygroundConversation() error = %v", err)
	}
	if conversation.MessageCount != len(messages) || len(conversation.Messages) != conversation.MessageCount {
		t.Fatalf("message count mismatch: summary=%d messages=%d", conversation.MessageCount, len(conversation.Messages))
	}
	if got := []string{conversation.Messages[0].ID, conversation.Messages[1].ID, conversation.Messages[2].ID}; !reflect.DeepEqual(got, []string{"msg_1", "msg_2", "msg_3"}) {
		t.Fatalf("message order = %v, want [msg_1 msg_2 msg_3]", got)
	}
	if conversation.Messages[1].RawResponse != nil || conversation.Messages[2].RawResponse != nil {
		t.Fatalf("empty raw response should remain nil: %#v %#v", conversation.Messages[1].RawResponse, conversation.Messages[2].RawResponse)
	}
	raw, ok := conversation.Messages[0].RawResponse.(map[string]any)
	if !ok || raw["answer"] != "ok" || raw["tokens"] != float64(3) {
		t.Fatalf("raw response = %#v, want decoded object", conversation.Messages[0].RawResponse)
	}
	if _, err := store.GetPlaygroundConversation(ctx, "other-actor", "conv_1"); !errors.Is(err, ports.ErrKeyNotFound) {
		t.Fatalf("actor-isolated lookup error = %v, want ErrKeyNotFound", err)
	}
	conversationID := "conv_1"
	if err := store.CreatePlaygroundRun(ctx, ports.PlaygroundRun{ID: "run_1", ConversationID: &conversationID, Actor: "admin", Channel: "wb", Model: "model-a", Status: "running", MessageCount: len(messages), CreatedAt: now}); err != nil {
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

func TestPurgeExpiredProvisionStateKeepsActiveSessions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/purge.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	now := time.Unix(1_800_000_000, 0)
	past, future := float64(now.Add(-time.Hour).Unix()), float64(now.Add(time.Hour).Unix())
	for _, row := range []struct {
		id      string
		status  string
		expires float64
	}{
		{"done-expired", "succeeded", past},
		{"failed-expired", "failed_terminal", past},
		{"pending-expired", "pending", past},
		{"done-live", "succeeded", future},
	} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO provision_sessions
			(channel, session_id, flow, status, created_at, expires_at, state, updated_at)
			VALUES ('wb', ?, 'qr-oauth', ?, ?, ?, 'x', ?)`, row.id, row.status, past, row.expires, past); err != nil {
			t.Fatalf("insert session %s: %v", row.id, err)
		}
	}
	for _, row := range []struct {
		key     string
		expires any
	}{{"old", past}, {"live", future}, {"forever", nil}} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO provision_idempotency
			(channel, operation, idempotency_key, session_id, expires_at, response, created_at, updated_at)
			VALUES ('wb', 'import', ?, '', ?, 'x', 0, 0)`, row.key, row.expires); err != nil {
			t.Fatalf("insert idempotency %s: %v", row.key, err)
		}
	}

	removed, err := store.PurgeExpiredProvisionState(ctx, now)
	if err != nil {
		t.Fatalf("PurgeExpiredProvisionState() error = %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3 (2 terminal sessions + 1 idempotency row)", removed)
	}
	var sessions, idempotency int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM provision_sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM provision_idempotency").Scan(&idempotency); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 || idempotency != 2 {
		t.Fatalf("remaining sessions=%d idempotency=%d, want 2 and 2", sessions, idempotency)
	}
}

func TestListRequestLogsAcceptsNullableColumns(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/nullable-logs.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	// Rows written by earlier versions leave channel/model/error_kind NULL.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO request_logs(ts, request_id, status) VALUES (unixepoch(), 'legacy-row', 200)`); err != nil {
		t.Fatal(err)
	}
	rows, total, err := store.ListRequestLogs(ctx, ports.LogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListRequestLogs() error = %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].RequestID != "legacy-row" || rows[0].ErrorKind != "" || rows[0].Channel != "" {
		t.Fatalf("ListRequestLogs() = %#v, %d", rows, total)
	}
}

// plainBox is a test SecretBox that stores plaintext.
type plainBox struct{}

func (plainBox) Encrypt(value string) (string, error) { return "plain:" + value, nil }
func (plainBox) Decrypt(value string) (string, error) { return value[len("plain:"):], nil }

func TestImportAccountsRenewsExistingCredential(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/import.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	item := ports.AccountImport{Channel: "grok", ID: "grok:build:u1", NativeID: "build:u1", Name: "u1", Kind: "oauth", Credentials: map[string]string{"access_token": "old"}}
	first, err := store.ImportAccounts(ctx, []ports.AccountImport{item}, plainBox{}, ports.AuditEvent{})
	if err != nil || first.Added != 1 {
		t.Fatalf("first import = %#v/%v", first, err)
	}
	same, err := store.ImportAccounts(ctx, []ports.AccountImport{item}, plainBox{}, ports.AuditEvent{})
	if err != nil || same.Skipped != 1 || same.Updated != 0 {
		t.Fatalf("identical import = %#v/%v", same, err)
	}
	item.Credentials = map[string]string{"access_token": "new"}
	renewed, err := store.ImportAccounts(ctx, []ports.AccountImport{item}, plainBox{}, ports.AuditEvent{})
	if err != nil || renewed.Updated != 1 || renewed.Added != 0 {
		t.Fatalf("renewed import = %#v/%v", renewed, err)
	}
	credentials, err := store.ReadCredential(ctx, "grok", "build:u1", plainBox{})
	if err != nil || credentials["access_token"] != "new" {
		t.Fatalf("credential after renew = %#v/%v", credentials, err)
	}
}
