package persistence

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/google/uuid"
)

const SchemaVersion = 11

var ErrDatabaseTooNew = errors.New("database schema is newer than this binary")

//go:embed migrations/*.sql
var migrationFiles embed.FS

type DB struct {
	db   *sql.DB
	path string
}

var _ ports.Storage = (*DB)(nil)
var _ ports.AuditWriter = (*DB)(nil)
var _ ports.KeyRepository = (*DB)(nil)
var _ ports.AccountSource = (*DB)(nil)
var _ ports.AdminQueryRepository = (*DB)(nil)
var _ ports.CatalogRepository = (*DB)(nil)
var _ ports.UserRepository = (*DB)(nil)
var _ ports.SystemRepository = (*DB)(nil)
var _ ports.AccountRepository = (*DB)(nil)
var _ ports.ProvisionRepository = (*DB)(nil)
var _ ports.CredentialRepository = (*DB)(nil)
var _ ports.PlaygroundRepository = (*DB)(nil)
var _ ports.AuditRepository = (*DB)(nil)
var _ ports.RequestRecorder = (*DB)(nil)

func Open(ctx context.Context, path string) (*DB, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := ensureParent(resolved); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", resolved)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	// SQLite serializes writes. A small bounded pool still permits concurrent
	// health/read requests without creating an unbounded connection fan-out.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	store := &DB{db: db, path: resolved}
	if err := store.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (d *DB) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 10000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := d.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite (%s): %w", statement, err)
		}
	}
	return nil
}

func (d *DB) Migrate(ctx context.Context) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	current, err := migrationVersion(ctx, tx)
	if err != nil {
		return err
	}
	if current > SchemaVersion {
		return fmt.Errorf("%w: found %d, supported through %d", ErrDatabaseTooNew, current, SchemaVersion)
	}

	if current < SchemaVersion {
		contents, err := migrationFiles.ReadFile("migrations/0011_baseline.sql")
		if err != nil {
			return fmt.Errorf("read baseline migration: %w", err)
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply baseline migration: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (?, unixepoch())",
			SchemaVersion,
		); err != nil {
			return fmt.Errorf("record baseline migration: %w", err)
		}
	}

	if err := ensureCompatibilityColumns(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET expires_at = NULL WHERE expires_at = 0"); err != nil {
		return fmt.Errorf("normalize account expiration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	committed = true
	return nil
}

func migrationVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var version sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

func ensureCompatibilityColumns(ctx context.Context, tx *sql.Tx) error {
	columns := []struct {
		table      string
		column     string
		definition string
	}{
		{"request_logs", "usage_reported", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_daily", "usage_reported_requests", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_daily", "usage_estimated_requests", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "usage_kind", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"playground_runs", "conversation_id", "TEXT"},
		{"playground_messages", "model", "TEXT NOT NULL DEFAULT ''"},
		{"api_keys", "key_encrypted", "TEXT"},
	}
	for _, item := range columns {
		rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+item.table+")")
		if err != nil {
			return fmt.Errorf("inspect %s: %w", item.table, err)
		}
		found := false
		for rows.Next() {
			var cid int
			var name, dataType string
			var notNull, pk int
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read %s columns: %w", item.table, err)
			}
			if name == item.column {
				found = true
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate %s columns: %w", item.table, err)
		}
		_ = rows.Close()
		if !found {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE "+item.table+" ADD COLUMN "+item.column+" "+item.definition); err != nil {
				return fmt.Errorf("add %s.%s: %w", item.table, item.column, err)
			}
		}
	}
	return nil
}

func (d *DB) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

func (d *DB) Snapshot(ctx context.Context) (ports.StorageSnapshot, error) {
	version, err := d.schemaVersion(ctx)
	if err != nil {
		return ports.StorageSnapshot{}, err
	}
	tableNames := []string{
		"account_runtime_state", "accounts", "api_key_rate_events", "api_keys", "audit_logs", "channel_runtime_state", "channels",
		"credentials", "media_assets", "models", "playground_conversations",
		"playground_messages", "playground_runs", "provision_idempotency",
		"provision_sessions", "request_logs", "route_targets", "routes", "schema_migrations",
		"settings", "usage_daily", "users",
	}
	sort.Strings(tableNames)
	tables := make(map[string]int64, len(tableNames))
	for _, table := range tableNames {
		var count int64
		if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			return ports.StorageSnapshot{}, fmt.Errorf("count %s: %w", table, err)
		}
		tables[table] = count
	}
	return ports.StorageSnapshot{SchemaVersion: version, Tables: tables}, nil
}

func (d *DB) schemaVersion(ctx context.Context) (int, error) {
	var value sql.NullInt64
	if err := d.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&value); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !value.Valid {
		return 0, nil
	}
	return int(value.Int64), nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) Path() string {
	return d.path
}

func (d *DB) SQLDB() *sql.DB { return d.db }

func (d *DB) RecordAudit(ctx context.Context, actor, action, target, detail, ip string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO audit_logs(ts, actor, action, target, detail, ip) VALUES (unixepoch(), ?, ?, ?, ?, ?)",
		actor, action, target, detail, ip,
	)
	if err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

func (d *DB) RecordRequest(ctx context.Context, record ports.RequestRecord) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin request record: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	ts := record.StartedAt.Unix()
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	if record.UsageKind == "" {
		record.UsageKind = "unknown"
	}
	if record.Model == "" {
		record.Model = record.Channel + "/" + record.UpstreamModel
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO request_logs
		(ts, request_id, channel, key_id, model, upstream_model, route_alias, fallback_depth, status, error_kind, error,
		stream, prompt_tokens, completion_tokens, usage_reported, usage_kind, ttft_ms, latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, record.RequestID, record.Channel, record.KeyID, record.Model, record.UpstreamModel, nullableStringValue(record.RouteAlias), record.FallbackDepth,
		record.Status, record.ErrorKind, record.Error, boolInt(record.Stream), record.PromptTokens,
		record.CompletionTokens, boolInt(record.UsageReported), record.UsageKind, record.TTFTMS, record.LatencyMS,
	); err != nil {
		return fmt.Errorf("insert request log: %w", err)
	}
	day := time.Unix(ts, 0).UTC().Format("2006-01-02")
	reported := 0
	estimated := 0
	if record.UsageReported {
		reported = 1
	} else if record.UsageKind == "estimated" {
		estimated = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_daily
		(day, channel, key_id, model, requests, prompt_tokens, completion_tokens,
		usage_reported_requests, usage_estimated_requests)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(day, channel, key_id, model) DO UPDATE SET
		requests = usage_daily.requests + excluded.requests,
		prompt_tokens = usage_daily.prompt_tokens + excluded.prompt_tokens,
		completion_tokens = usage_daily.completion_tokens + excluded.completion_tokens,
		usage_reported_requests = usage_daily.usage_reported_requests + excluded.usage_reported_requests,
		usage_estimated_requests = usage_daily.usage_estimated_requests + excluded.usage_estimated_requests`,
		day, record.Channel, record.KeyID, record.Model, record.PromptTokens, record.CompletionTokens, reported, estimated,
	); err != nil {
		return fmt.Errorf("upsert daily usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit request record: %w", err)
	}
	committed = true
	return nil
}

func (d *DB) Candidates(ctx context.Context, channel, model string, requireCredentials bool) ([]ports.AccountCandidate, error) {
	_ = model
	credentialFilter := ""
	if requireCredentials {
		credentialFilter = `AND EXISTS (SELECT 1 FROM credentials c WHERE c.channel = a.channel AND c.account_id = a.native_id)`
	}
	rows, err := d.db.QueryContext(ctx, `SELECT a.id, a.native_id, a.channel, a.priority
		FROM accounts a LEFT JOIN account_runtime_state r ON r.account_id = a.id
		WHERE a.channel = ? AND a.enabled = 1
		AND COALESCE(a.status_override, a.status) IN ('ready', 'busy', 'cooldown', 'limited')
		AND (r.cooldown_until IS NULL OR r.cooldown_until <= unixepoch())
		AND (r.breaker_until IS NULL OR r.breaker_until <= unixepoch()) `+credentialFilter+`
		ORDER BY COALESCE(r.updated_at, 0), a.priority DESC, a.name, a.id`, channel)
	if err != nil {
		return nil, fmt.Errorf("list account candidates: %w", err)
	}
	defer rows.Close()
	result := make([]ports.AccountCandidate, 0)
	for rows.Next() {
		var candidate ports.AccountCandidate
		if err := rows.Scan(&candidate.ID, &candidate.NativeID, &candidate.Channel, &candidate.Priority); err != nil {
			return nil, fmt.Errorf("scan account candidate: %w", err)
		}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate account candidates: %w", err)
	}
	return result, nil
}

func (d *DB) ListAccounts(ctx context.Context, query ports.AccountQuery) ([]ports.AccountRecord, int, error) {
	where := []string{}
	args := []any{}
	if query.Channel != "" {
		where = append(where, "a.channel = ?")
		args = append(args, query.Channel)
	}
	if query.Status != "" {
		where = append(where, "COALESCE(a.status_override, a.status) = ?")
		args = append(args, query.Status)
	}
	if query.Search != "" {
		value := "%" + escapeLike(query.Search) + "%"
		where = append(where, `(a.id LIKE ? ESCAPE '\' OR a.name LIKE ? ESCAPE '')`)
		args = append(args, value, value)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts a"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page := query.Page
	if page < 1 {
		page = 1
	}
	size := query.PageSize
	if size < 1 {
		size = 50
	}
	if size > 200 {
		size = 200
	}
	listArgs := append(append([]any(nil), args...), size, (page-1)*size)
	rows, err := d.db.QueryContext(ctx, `SELECT a.id, a.channel, a.name, a.kind, a.tier,
		COALESCE(a.status_override, a.status), a.enabled, a.quota_used, a.quota_total, a.quota_unit,
		a.expires_at, a.updated_at, r.success_count, r.fail_count, r.consecutive_failures,
		r.cooldown_until, r.breaker_until, r.last_status, r.last_error_kind, r.updated_at
		FROM accounts a LEFT JOIN account_runtime_state r ON r.account_id = a.id`+whereSQL+" ORDER BY a.updated_at DESC, a.id LIMIT ? OFFSET ?", listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]ports.AccountRecord, 0)
	for rows.Next() {
		var item ports.AccountRecord
		var enabled int
		var cooldown, expires, runtimeUpdated sql.NullInt64
		var success, fail, consecutive int
		var breaker, lastStatus sql.NullInt64
		var lastError sql.NullString
		if err := rows.Scan(&item.ID, &item.Channel, &item.Name, &item.Kind, &item.Tier, &item.Status, &enabled, &item.QuotaUsed, &item.QuotaTotal, &item.QuotaUnit, &expires, &item.UpdatedAt, &success, &fail, &consecutive, &cooldown, &breaker, &lastStatus, &lastError, &runtimeUpdated); err != nil {
			return nil, 0, err
		}
		item.Enabled = enabled != 0
		if expires.Valid {
			v := expires.Int64
			item.ExpiresAt = &v
		}
		if cooldown.Valid {
			v := cooldown.Int64
			item.CooldownUntil = &v
		}
		item.GatewayRuntime = map[string]any{"state": "unobserved", "success_count": success, "fail_count": fail, "consecutive_failures": consecutive, "cooldown_until": nil, "breaker_until": nil, "last_status": nil, "last_error_kind": nil, "updated_at": nil, "retry_after": nil}
		if runtimeUpdated.Valid {
			state := "closed"
			if cooldown.Valid && cooldown.Int64 > time.Now().Unix() {
				state = "cooldown"
			}
			if breaker.Valid && breaker.Int64 > time.Now().Unix() {
				state = "breaker_open"
			}
			item.GatewayRuntime["state"] = state
			item.GatewayRuntime["updated_at"] = runtimeUpdated.Int64
			if cooldown.Valid {
				item.GatewayRuntime["cooldown_until"] = cooldown.Int64
			}
			if breaker.Valid {
				item.GatewayRuntime["breaker_until"] = breaker.Int64
			}
			if lastStatus.Valid {
				item.GatewayRuntime["last_status"] = lastStatus.Int64
			}
			if lastError.Valid {
				item.GatewayRuntime["last_error_kind"] = lastError.String
			}
		}
		result = append(result, item)
	}
	return result, total, rows.Err()
}

func (d *DB) SetAccountEnabled(ctx context.Context, id string, enabled bool, audit ports.AuditEvent) (ports.AccountRecord, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.AccountRecord{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM accounts WHERE id = ?", id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ports.AccountRecord{}, ports.ErrKeyNotFound
	} else if err != nil {
		return ports.AccountRecord{}, err
	}
	statusOverride := any(nil)
	if !enabled {
		statusOverride = "disabled"
	}
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET enabled = ?, status_override = ?, updated_at = unixepoch() WHERE id = ?", boolInt(enabled), statusOverride, id); err != nil {
		return ports.AccountRecord{}, err
	}
	audit.Target = id
	if audit.Action == "" {
		if enabled {
			audit.Action = "enable_account"
		} else {
			audit.Action = "disable_account"
		}
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.AccountRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.AccountRecord{}, err
	}
	committed = true
	rows, total, err := d.ListAccounts(ctx, ports.AccountQuery{Page: 1, PageSize: 1, Search: id})
	if err != nil || total == 0 {
		return ports.AccountRecord{}, err
	}
	return rows[0], nil
}

func (d *DB) DeleteAccount(ctx context.Context, id string, audit ports.AuditEvent) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var nativeID string
	if err := tx.QueryRowContext(ctx, "SELECT native_id FROM accounts WHERE id = ?", id).Scan(&nativeID); errors.Is(err, sql.ErrNoRows) {
		return ports.ErrKeyNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM credentials WHERE account_id = ?", nativeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_runtime_state WHERE account_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id); err != nil {
		return err
	}
	audit.Target = id
	if audit.Action == "" {
		audit.Action = "delete_account"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (d *DB) DeleteAccounts(ctx context.Context, ids []string, audit ports.AuditEvent) (ports.AccountBatchDeleteResult, error) {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	result := ports.AccountBatchDeleteResult{Requested: len(unique), Deleted: []ports.AccountDeleteResult{}, Failed: []ports.AccountDeleteFailure{}}
	if len(unique) == 0 {
		return result, fmt.Errorf("ids is required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.AccountBatchDeleteResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, id := range unique {
		var channel, nativeID string
		if err := tx.QueryRowContext(ctx, "SELECT channel, native_id FROM accounts WHERE id = ?", id).Scan(&channel, &nativeID); errors.Is(err, sql.ErrNoRows) {
			result.Failed = append(result.Failed, ports.AccountDeleteFailure{ID: id, Error: "account not found"})
			continue
		} else if err != nil {
			return ports.AccountBatchDeleteResult{}, err
		}
		credentialsDeleted := false
		if deletion, err := tx.ExecContext(ctx, "DELETE FROM credentials WHERE account_id = ?", nativeID); err != nil {
			return ports.AccountBatchDeleteResult{}, err
		} else {
			count, _ := deletion.RowsAffected()
			credentialsDeleted = count > 0
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM account_runtime_state WHERE account_id = ?", id); err != nil {
			return ports.AccountBatchDeleteResult{}, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id); err != nil {
			return ports.AccountBatchDeleteResult{}, err
		}
		event := audit
		event.Action = "delete_account"
		event.Target = id
		if err := insertAuditTx(ctx, tx, event, 0); err != nil {
			return ports.AccountBatchDeleteResult{}, err
		}
		result.Deleted = append(result.Deleted, ports.AccountDeleteResult{ID: id, Channel: channel, CredentialsDeleted: credentialsDeleted})
	}
	if err := tx.Commit(); err != nil {
		return ports.AccountBatchDeleteResult{}, err
	}
	committed = true
	return result, nil
}

func (d *DB) ImportAccounts(ctx context.Context, items []ports.AccountImport, box ports.SecretBox, audit ports.AuditEvent) (ports.AccountImportResult, error) {
	if box == nil {
		return ports.AccountImportResult{}, fmt.Errorf("credential encryption is unavailable")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.AccountImportResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	result := ports.AccountImportResult{Errors: []string{}, Accounts: []ports.AccountRecord{}}
	for _, item := range items {
		if item.Channel == "" || item.NativeID == "" || item.Name == "" || len(item.Credentials) == 0 {
			result.Errors = append(result.Errors, "account item is invalid")
			continue
		}
		var existing int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM accounts WHERE channel = ? AND native_id = ?", item.Channel, item.NativeID).Scan(&existing); err == nil {
			result.Skipped++
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return ports.AccountImportResult{}, err
		}
		payload, _ := json.Marshal(item.Credentials)
		encrypted, err := box.Encrypt(string(payload))
		if err != nil {
			return ports.AccountImportResult{}, err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(id,channel,native_id,name,kind,status,enabled,ext,created_at,updated_at) VALUES(?,?,?,?,?,'ready',1,?, ?, ?)`, item.ID, item.Channel, item.NativeID, item.Name, item.Kind, "{}", now, now); err != nil {
			return ports.AccountImportResult{}, err
		}
		ref := fmt.Sprintf("cred_%s_%s", item.Channel, uuid.NewString())
		if _, err := tx.ExecContext(ctx, `INSERT INTO credentials(credential_ref,channel,account_id,encrypted_payload,created_at,updated_at) VALUES(?,?,?,?,?,?)`, ref, item.Channel, item.NativeID, encrypted, now, now); err != nil {
			return ports.AccountImportResult{}, err
		}
		result.Added++
		result.Accounts = append(result.Accounts, ports.AccountRecord{ID: item.ID, Channel: item.Channel, NativeID: item.NativeID, Name: item.Name, Kind: item.Kind, Status: "ready", Enabled: true, QuotaUnit: "none", UpdatedAt: now})
	}
	audit.Action = "import_account"
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.AccountImportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.AccountImportResult{}, err
	}
	committed = true
	return result, nil
}

func (d *DB) SaveProvisionSession(ctx context.Context, session ports.ProvisionSession, box ports.SecretBox) error {
	if box == nil {
		return fmt.Errorf("provision encryption is unavailable")
	}
	payload, err := json.Marshal(session.State)
	if err != nil {
		return err
	}
	encrypted, err := box.Encrypt(string(payload))
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO provision_sessions
		(channel, session_id, flow, status, idempotency_key, created_at, expires_at, state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, unixepoch())
		ON CONFLICT(channel, session_id) DO UPDATE SET
		flow=excluded.flow, status=excluded.status, idempotency_key=excluded.idempotency_key,
		created_at=excluded.created_at, expires_at=excluded.expires_at, state=excluded.state, updated_at=excluded.updated_at`,
		session.Channel, session.SessionID, session.Flow, session.Status, session.IdempotencyKey,
		session.CreatedAt, session.ExpiresAt, encrypted)
	return err
}

func (d *DB) LoadProvisionSession(ctx context.Context, channel, sessionID string, box ports.SecretBox) (ports.ProvisionSession, error) {
	if box == nil {
		return ports.ProvisionSession{}, fmt.Errorf("provision encryption is unavailable")
	}
	var session ports.ProvisionSession
	var encrypted string
	var expires, created float64
	err := d.db.QueryRowContext(ctx, `SELECT flow, status, idempotency_key, created_at, expires_at, state
		FROM provision_sessions WHERE channel = ? AND session_id = ?`, channel, sessionID).Scan(&session.Flow, &session.Status, &session.IdempotencyKey, &created, &expires, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ProvisionSession{}, ports.ErrKeyNotFound
	}
	if err != nil {
		return ports.ProvisionSession{}, err
	}
	plaintext, err := box.Decrypt(encrypted)
	if err != nil {
		return ports.ProvisionSession{}, err
	}
	if err := json.Unmarshal([]byte(plaintext), &session.State); err != nil {
		return ports.ProvisionSession{}, err
	}
	session.Channel, session.SessionID, session.CreatedAt, session.ExpiresAt = channel, sessionID, created, expires
	return session, nil
}

func (d *DB) DeleteProvisionSession(ctx context.Context, channel, sessionID string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM provision_sessions WHERE channel = ? AND session_id = ?", channel, sessionID)
	return err
}

func (d *DB) GetProvisionIdempotency(ctx context.Context, channel, operation, key string, box ports.SecretBox) (ports.ProvisionIdempotency, bool, error) {
	if box == nil {
		return ports.ProvisionIdempotency{}, false, fmt.Errorf("provision encryption is unavailable")
	}
	var sessionID, encrypted string
	var expires sql.NullFloat64
	err := d.db.QueryRowContext(ctx, `SELECT session_id, expires_at, response FROM provision_idempotency
		WHERE channel = ? AND operation = ? AND idempotency_key = ?`, channel, operation, key).Scan(&sessionID, &expires, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ProvisionIdempotency{}, false, nil
	}
	if err != nil {
		return ports.ProvisionIdempotency{}, false, err
	}
	if expires.Valid && expires.Float64 <= float64(time.Now().Unix()) {
		_, _ = d.db.ExecContext(ctx, "DELETE FROM provision_idempotency WHERE channel = ? AND operation = ? AND idempotency_key = ?", channel, operation, key)
		return ports.ProvisionIdempotency{}, false, nil
	}
	plaintext, err := box.Decrypt(encrypted)
	if err != nil {
		return ports.ProvisionIdempotency{}, false, err
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(plaintext), &response); err != nil {
		return ports.ProvisionIdempotency{}, false, err
	}
	return ports.ProvisionIdempotency{Response: response, SessionID: sessionID, ExpiresAt: expires.Float64}, true, nil
}

func (d *DB) PutProvisionIdempotency(ctx context.Context, channel, operation, key, sessionID string, response map[string]any, expiresAt float64, box ports.SecretBox) error {
	if box == nil {
		return fmt.Errorf("provision encryption is unavailable")
	}
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	encrypted, err := box.Encrypt(string(payload))
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO provision_idempotency(channel, operation, idempotency_key, session_id, expires_at, response, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, unixepoch(), unixepoch())
		ON CONFLICT(channel, operation, idempotency_key) DO UPDATE SET session_id=excluded.session_id, expires_at=excluded.expires_at, response=excluded.response, updated_at=excluded.updated_at`, channel, operation, key, sessionID, expiresAt, encrypted)
	return err
}

func (d *DB) ListPlaygroundConversations(ctx context.Context, actor string, page, pageSize int) ([]ports.PlaygroundConversationSummary, int, error) {
	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM playground_conversations WHERE actor = ?", actor).Scan(&total); err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	rows, err := d.db.QueryContext(ctx, `SELECT c.id,c.actor,c.title,c.channel,c.model,(SELECT COUNT(*) FROM playground_messages m WHERE m.conversation_id=c.id),c.created_at,c.updated_at FROM playground_conversations c WHERE c.actor = ? ORDER BY c.updated_at DESC,c.id DESC LIMIT ? OFFSET ?`, actor, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []ports.PlaygroundConversationSummary{}
	for rows.Next() {
		var item ports.PlaygroundConversationSummary
		if err := rows.Scan(&item.ID, &item.Actor, &item.Title, &item.Channel, &item.Model, &item.MessageCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, 0, err
		}
		result = append(result, item)
	}
	return result, total, rows.Err()
}

func (d *DB) GetPlaygroundConversation(ctx context.Context, actor, id string) (ports.PlaygroundConversation, error) {
	var item ports.PlaygroundConversation
	if err := d.db.QueryRowContext(ctx, `SELECT c.id,c.actor,c.title,c.channel,c.model,(SELECT COUNT(*) FROM playground_messages m WHERE m.conversation_id=c.id),c.created_at,c.updated_at FROM playground_conversations c WHERE c.id = ? AND c.actor = ?`, id, actor).Scan(&item.ID, &item.Actor, &item.Title, &item.Channel, &item.Model, &item.MessageCount, &item.CreatedAt, &item.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return ports.PlaygroundConversation{}, ports.ErrKeyNotFound
	} else if err != nil {
		return ports.PlaygroundConversation{}, err
	}
	return item, nil
}

func (d *DB) DeletePlaygroundConversation(ctx context.Context, actor, id string, audit ports.AuditEvent) (int, error) {
	if _, err := d.GetPlaygroundConversation(ctx, actor, id); err != nil {
		return 0, err
	}
	var count int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM playground_runs WHERE conversation_id = ?", id).Scan(&count); err != nil {
		return 0, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM playground_runs WHERE conversation_id = ?", id); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM playground_conversations WHERE id = ?", id); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	audit.Target = id
	if audit.Action == "" {
		audit.Action = "delete_playground_conversation"
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit_logs(ts,actor,action,target,detail,ip) VALUES(unixepoch(),?,?,?,?,?)", audit.Actor, audit.Action, audit.Target, audit.Detail, audit.IP); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (d *DB) ListPlaygroundRuns(ctx context.Context, page, pageSize int) ([]ports.PlaygroundRun, int, error) {
	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM playground_runs").Scan(&total); err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id,conversation_id,actor,channel,model,status,message_count,request_bytes,response_status,error_code,created_at,completed_at FROM playground_runs ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []ports.PlaygroundRun{}
	for rows.Next() {
		var item ports.PlaygroundRun
		var conversationID, errorCode sql.NullString
		var responseStatus, completed sql.NullInt64
		if err := rows.Scan(&item.ID, &conversationID, &item.Actor, &item.Channel, &item.Model, &item.Status, &item.MessageCount, &item.RequestBytes, &responseStatus, &errorCode, &item.CreatedAt, &completed); err != nil {
			return nil, 0, err
		}
		if conversationID.Valid {
			item.ConversationID = &conversationID.String
		}
		if responseStatus.Valid {
			value := int(responseStatus.Int64)
			item.ResponseStatus = &value
		}
		if errorCode.Valid {
			item.ErrorCode = &errorCode.String
		}
		if completed.Valid {
			item.CompletedAt = &completed.Int64
		}
		result = append(result, item)
	}
	return result, total, rows.Err()
}

func (d *DB) CreatePlaygroundConversation(ctx context.Context, item ports.PlaygroundConversationSummary) error {
	_, err := d.db.ExecContext(ctx, "INSERT INTO playground_conversations(id,actor,title,channel,model,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", item.ID, item.Actor, item.Title, item.Channel, item.Model, item.CreatedAt, item.UpdatedAt)
	return err
}
func (d *DB) AddPlaygroundMessage(ctx context.Context, conversationID string, item ports.PlaygroundMessage) error {
	raw := ""
	if item.RawResponse != nil {
		encoded, _ := json.Marshal(item.RawResponse)
		raw = string(encoded)
	}
	_, err := d.db.ExecContext(ctx, "INSERT INTO playground_messages(id,conversation_id,role,content,model,raw_response,created_at) VALUES(?,?,?,?,?,?,?)", item.ID, conversationID, item.Role, item.Content, item.Model, raw, item.CreatedAt)
	return err
}
func (d *DB) CreatePlaygroundRun(ctx context.Context, item ports.PlaygroundRun) error {
	_, err := d.db.ExecContext(ctx, "INSERT INTO playground_runs(id,conversation_id,actor,channel,model,status,message_count,request_bytes,created_at) VALUES(?,?,?,?,?,?,?,?,?)", item.ID, item.ConversationID, item.Actor, item.Channel, item.Model, item.Status, item.MessageCount, item.RequestBytes, item.CreatedAt)
	return err
}
func (d *DB) FinishPlaygroundRun(ctx context.Context, id string, status string, responseStatus int, errCode string, completed time.Time) error {
	var code any
	if errCode != "" {
		code = errCode
	}
	_, err := d.db.ExecContext(ctx, "UPDATE playground_runs SET status=?,response_status=?,error_code=?,completed_at=? WHERE id=?", status, responseStatus, code, completed.Unix(), id)
	return err
}

func (d *DB) ListAudit(ctx context.Context, query ports.AuditQuery) ([]ports.AuditRecord, int, error) {
	where := []string{}
	args := []any{}
	for column, value := range map[string]string{"actor": query.Actor, "action": query.Action, "target": query.Target} {
		if value != "" {
			where = append(where, column+" = ?")
			args = append(args, value)
		}
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_logs"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page := query.Page
	if page < 1 {
		page = 1
	}
	size := query.PageSize
	if size < 1 {
		size = 50
	}
	if size > 200 {
		size = 200
	}
	args = append(args, size, (page-1)*size)
	rows, err := d.db.QueryContext(ctx, "SELECT id,ts,actor,action,target,detail FROM audit_logs"+whereSQL+" ORDER BY ts DESC,id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []ports.AuditRecord{}
	for rows.Next() {
		var item ports.AuditRecord
		if err := rows.Scan(&item.ID, &item.Timestamp, &item.Actor, &item.Action, &item.Target, &item.Detail); err != nil {
			return nil, 0, err
		}
		result = append(result, item)
	}
	return result, total, rows.Err()
}

func (d *DB) ReadCredential(ctx context.Context, channel, accountID string, box ports.SecretBox) (map[string]string, error) {
	if box == nil {
		return nil, fmt.Errorf("credential encryption is unavailable")
	}
	var encrypted string
	err := d.db.QueryRowContext(ctx, "SELECT encrypted_payload FROM credentials WHERE channel = ? AND account_id = ?", channel, accountID).Scan(&encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("credential not found")
	}
	if err != nil {
		return nil, fmt.Errorf("read credential: %w", err)
	}
	plaintext, err := box.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(plaintext), &payload); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	result := make(map[string]string, len(payload))
	for key, value := range payload {
		if text, ok := value.(string); ok {
			result[key] = text
		}
	}
	return result, nil
}

func (d *DB) UpdateCredential(ctx context.Context, channel, accountID string, credentials map[string]string, box ports.SecretBox, audit ports.AuditEvent) error {
	if box == nil {
		return fmt.Errorf("credential encryption is unavailable")
	}
	payload, err := json.Marshal(credentials)
	if err != nil {
		return fmt.Errorf("encode credential: %w", err)
	}
	encrypted, err := box.Encrypt(string(payload))
	if err != nil {
		return fmt.Errorf("encrypt credential: %w", err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin credential update: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM credentials WHERE channel = ? AND account_id = ?", channel, accountID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("credential not found")
	} else if err != nil {
		return err
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, "UPDATE credentials SET encrypted_payload = ?, updated_at = ? WHERE channel = ? AND account_id = ?", encrypted, now, channel, accountID); err != nil {
		return fmt.Errorf("update credential: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET updated_at = ? WHERE channel = ? AND native_id = ?", now, channel, accountID); err != nil {
		return fmt.Errorf("update account timestamp: %w", err)
	}
	audit.Target = channel + ":" + accountID
	if audit.Action == "" {
		audit.Action = "refresh_credential"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit credential update: %w", err)
	}
	committed = true
	return nil
}

func (d *DB) ListRequestLogs(ctx context.Context, query ports.LogQuery) ([]ports.RequestLog, int, error) {
	where := make([]string, 0, 8)
	args := make([]any, 0, 10)
	if query.RequestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, query.RequestID)
	}
	if query.Channel != "" {
		where = append(where, "channel = ?")
		args = append(args, query.Channel)
	}
	if query.Model != "" {
		where = append(where, "model LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(query.Model)+"%")
	}
	if query.Status != nil {
		where = append(where, "status = ?")
		args = append(args, *query.Status)
	}
	if query.ErrorKind != "" {
		where = append(where, "error_kind = ?")
		args = append(args, query.ErrorKind)
	}
	if query.Stream != nil {
		where = append(where, "stream = ?")
		args = append(args, boolInt(*query.Stream))
	}
	if query.From != nil {
		where = append(where, "ts >= ?")
		args = append(args, *query.From)
	}
	if query.To != nil {
		where = append(where, "ts < ?")
		args = append(args, *query.To)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count request logs: %w", err)
	}
	pageSize := query.PageSize
	if pageSize < 1 {
		pageSize = 50
	}
	args = append(args, pageSize, (query.Page-1)*pageSize)
	rows, err := d.db.QueryContext(ctx, `SELECT id, ts, request_id, channel, key_id, model, upstream_model, route_alias, fallback_depth, status,
		error_kind, stream, prompt_tokens, completion_tokens, usage_reported, usage_kind, ttft_ms, latency_ms
		FROM request_logs`+whereSQL+" ORDER BY id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list request logs: %w", err)
	}
	defer rows.Close()
	result := make([]ports.RequestLog, 0)
	for rows.Next() {
		var item ports.RequestLog
		var keyID, ttft sql.NullInt64
		var upstreamModel, routeAlias sql.NullString
		var stream, reported int
		if err := rows.Scan(&item.ID, &item.Timestamp, &item.RequestID, &item.Channel, &keyID, &item.Model, &upstreamModel, &routeAlias, &item.FallbackDepth, &item.Status, &item.ErrorKind, &stream, &item.PromptTokens, &item.CompletionTokens, &reported, &item.UsageKind, &ttft, &item.LatencyMS); err != nil {
			return nil, 0, fmt.Errorf("scan request log: %w", err)
		}
		item.Stream, item.UsageReported = stream != 0, reported != 0
		if keyID.Valid {
			value := keyID.Int64
			item.KeyID = &value
		}
		if upstreamModel.Valid {
			value := upstreamModel.String
			item.UpstreamModel = &value
		}
		if routeAlias.Valid {
			value := routeAlias.String
			item.RouteAlias = &value
		}
		if ttft.Valid {
			value := ttft.Int64
			item.TTFTMS = &value
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate request logs: %w", err)
	}
	return result, total, nil
}

func (d *DB) ClearExpiredLogs(ctx context.Context, logRetentionDays, usageRetentionDays int, audit ports.AuditEvent) (ports.ClearLogsResult, error) {
	if logRetentionDays < 1 || usageRetentionDays < 1 || usageRetentionDays < logRetentionDays {
		return ports.ClearLogsResult{}, fmt.Errorf("invalid retention days")
	}
	now := time.Now().UTC()
	logCutoff := now.Add(-time.Duration(logRetentionDays) * 24 * time.Hour)
	usageCutoff := now.AddDate(0, 0, -(usageRetentionDays - 1)).Format("2006-01-02")
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.ClearLogsResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var requestLogs, usageDaily int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs WHERE ts < ?", logCutoff.Unix()).Scan(&requestLogs); err != nil {
		return ports.ClearLogsResult{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_daily WHERE day < ?", usageCutoff).Scan(&usageDaily); err != nil {
		return ports.ClearLogsResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM request_logs WHERE ts < ?", logCutoff.Unix()); err != nil {
		return ports.ClearLogsResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM usage_daily WHERE day < ?", usageCutoff); err != nil {
		return ports.ClearLogsResult{}, err
	}
	audit.Action = "clear_logs"
	audit.Target = "retention"
	audit.Detail = fmt.Sprintf("request_logs=%d usage_daily=%d log_cutoff=%s usage_cutoff_day=%s", requestLogs, usageDaily, logCutoff.Format(time.RFC3339), usageCutoff)
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.ClearLogsResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.ClearLogsResult{}, err
	}
	committed = true
	return ports.ClearLogsResult{RequestLogsDeleted: requestLogs, UsageDailyDeleted: usageDaily, LogCutoff: logCutoff.Format(time.RFC3339), UsageCutoffDay: usageCutoff, LogRetentionDays: logRetentionDays, UsageRetentionDays: usageRetentionDays}, nil
}

func (d *DB) UsageSummary(ctx context.Context, days int) (ports.UsageSummary, error) {
	var summary ports.UsageSummary
	if err := d.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(usage_reported_requests), 0),
		COALESCE(SUM(usage_estimated_requests), 0), COALESCE(MIN(day), date('now')), COALESCE(MAX(day), date('now'))
		FROM usage_daily WHERE day >= date('now', ? || ' day')`, fmt.Sprintf("-%d", days-1)).Scan(
		&summary.Requests, &summary.PromptTokens, &summary.CompletionTokens,
		&summary.UsageReportedRequests, &summary.UsageEstimatedRequests, &summary.From, &summary.To,
	); err != nil {
		return ports.UsageSummary{}, fmt.Errorf("usage summary: %w", err)
	}
	return summary, nil
}

func (d *DB) UsageRows(ctx context.Context, days int, dimension string) ([]ports.UsageRow, error) {
	group := map[string]string{"daily": "day", "channel": "channel", "model": "model", "key": "key_id"}[dimension]
	selectSQL := group
	if dimension == "key" {
		selectSQL = "key_id"
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+selectSQL+`, SUM(requests), SUM(prompt_tokens), SUM(completion_tokens),
		SUM(usage_reported_requests), SUM(usage_estimated_requests)
		FROM usage_daily WHERE day >= date('now', ? || ' day') GROUP BY `+group+` ORDER BY `+group, fmt.Sprintf("-%d", days-1))
	if err != nil {
		return nil, fmt.Errorf("usage rows: %w", err)
	}
	defer rows.Close()
	result := make([]ports.UsageRow, 0)
	for rows.Next() {
		var value sql.NullString
		var row ports.UsageRow
		if err := rows.Scan(&value, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.UsageReportedRequests, &row.UsageEstimatedRequests); err != nil {
			return nil, fmt.Errorf("scan usage row: %w", err)
		}
		if dimension == "daily" {
			row.Day = value.String
		} else if dimension == "channel" {
			row.Channel = value.String
		} else if dimension == "model" {
			row.Model = value.String
		} else if value.Valid {
			var keyID int64
			if _, err := fmt.Sscan(value.String, &keyID); err == nil {
				row.KeyID = &keyID
			}
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage rows: %w", err)
	}
	return result, nil
}

func (d *DB) ListModels(ctx context.Context, query ports.ModelQuery) ([]ports.ModelRecord, int, []string, error) {
	where := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if query.Channel != "" {
		where = append(where, "channel = ?")
		args = append(args, query.Channel)
	}
	if query.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, query.Kind)
	}
	if query.Enabled != nil {
		where = append(where, "enabled = ?")
		if *query.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if query.Search != "" {
		value := "%" + escapeLike(query.Search) + "%"
		where = append(where, `(id LIKE ? ESCAPE '\' OR display_name LIKE ? ESCAPE '')`)
		args = append(args, value, value)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM models"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, nil, fmt.Errorf("count models: %w", err)
	}
	page := query.Page
	if page < 1 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	listArgs := append(append([]any(nil), args...), pageSize, (page-1)*pageSize)
	rows, err := d.db.QueryContext(ctx, `SELECT id, channel, upstream_id, display_name, kind, caps,
		context_window, max_output, multiplier, enabled FROM models`+whereSQL+" ORDER BY channel, display_name, id LIMIT ? OFFSET ?", listArgs...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	result := make([]ports.ModelRecord, 0)
	for rows.Next() {
		item, err := scanModel(rows)
		if err != nil {
			return nil, 0, nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, fmt.Errorf("iterate models: %w", err)
	}
	kindRows, err := d.db.QueryContext(ctx, "SELECT DISTINCT kind FROM models WHERE kind != '' ORDER BY kind")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list model kinds: %w", err)
	}
	defer kindRows.Close()
	kinds := make([]string, 0)
	for kindRows.Next() {
		var kind string
		if err := kindRows.Scan(&kind); err != nil {
			return nil, 0, nil, err
		}
		kinds = append(kinds, kind)
	}
	return result, total, kinds, nil
}

func (d *DB) SyncModels(ctx context.Context, channel string, models []ports.ModelDescriptor, audit ports.AuditEvent) (int, error) {
	if strings.TrimSpace(channel) == "" {
		return 0, fmt.Errorf("model channel is required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin model sync: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	count := 0
	for _, model := range models {
		if strings.TrimSpace(model.UpstreamID) == "" {
			continue
		}
		caps, err := json.Marshal(model.Capabilities)
		if err != nil {
			return 0, fmt.Errorf("encode model capabilities: %w", err)
		}
		id := channel + "/" + model.UpstreamID
		kind := "chat"
		if len(model.Capabilities) > 0 && !containsString(model.Capabilities, "chat") {
			kind = model.Capabilities[0]
		}
		name := model.DisplayName
		if strings.TrimSpace(name) == "" {
			name = model.UpstreamID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO models(id, channel, upstream_id, display_name, kind, caps, multiplier, enabled)
			VALUES (?, ?, ?, ?, ?, ?, 1, 1)
			ON CONFLICT(id) DO UPDATE SET channel=excluded.channel, upstream_id=excluded.upstream_id,
			display_name=excluded.display_name, kind=excluded.kind, caps=excluded.caps`,
			id, channel, model.UpstreamID, name, kind, string(caps)); err != nil {
			return 0, fmt.Errorf("upsert model %s: %w", id, err)
		}
		count++
	}
	audit.Target = channel
	if audit.Action == "" {
		audit.Action = "refresh_models"
	}
	audit.Detail = fmt.Sprintf("models=%d", count)
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit model sync: %w", err)
	}
	committed = true
	return count, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (d *DB) UpdateModelEnabled(ctx context.Context, id string, enabled bool, audit ports.AuditEvent) (ports.ModelRecord, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.ModelRecord{}, fmt.Errorf("begin model update: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM models WHERE id = ?", id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ports.ModelRecord{}, ports.ErrKeyNotFound
	} else if err != nil {
		return ports.ModelRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE models SET enabled = ? WHERE id = ?", boolInt(enabled), id); err != nil {
		return ports.ModelRecord{}, fmt.Errorf("update model: %w", err)
	}
	audit.Target = id
	if audit.Action == "" {
		audit.Action = "set_model_enabled"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.ModelRecord{}, err
	}
	item, err := scanModel(tx.QueryRowContext(ctx, `SELECT id, channel, upstream_id, display_name, kind, caps, context_window, max_output, multiplier, enabled FROM models WHERE id = ?`, id))
	if err != nil {
		return ports.ModelRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.ModelRecord{}, err
	}
	committed = true
	return item, nil
}

func (d *DB) ListRoutes(ctx context.Context) ([]ports.RouteRecord, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT alias, strategy, enabled, created_at FROM routes ORDER BY alias")
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	defer rows.Close()
	result := make([]ports.RouteRecord, 0)
	for rows.Next() {
		var item ports.RouteRecord
		var enabled int
		if err := rows.Scan(&item.Alias, &item.Strategy, &enabled, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		item.Targets, err = d.routeTargets(ctx, item.Alias)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (d *DB) routeTargets(ctx context.Context, alias string) ([]ports.RouteTarget, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT position, channel, model, weight FROM route_targets WHERE alias = ? ORDER BY position", alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ports.RouteTarget, 0)
	for rows.Next() {
		var item ports.RouteTarget
		if err := rows.Scan(&item.Position, &item.Channel, &item.Model, &item.Weight); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (d *DB) UpsertRoute(ctx context.Context, alias string, input ports.RouteInput, audit ports.AuditEvent) (ports.RouteRecord, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.RouteRecord{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO routes(alias, strategy, enabled, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(alias) DO UPDATE SET strategy=excluded.strategy, enabled=excluded.enabled`, alias, input.Strategy, boolInt(input.Enabled), now); err != nil {
		return ports.RouteRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM route_targets WHERE alias = ?", alias); err != nil {
		return ports.RouteRecord{}, err
	}
	for index, target := range input.Targets {
		if _, err := tx.ExecContext(ctx, "INSERT INTO route_targets(alias, position, channel, model, weight) VALUES (?, ?, ?, ?, ?)", alias, index, target.Channel, target.Model, target.Weight); err != nil {
			return ports.RouteRecord{}, err
		}
	}
	audit.Target = alias
	if audit.Action == "" {
		audit.Action = "upsert_route"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.RouteRecord{}, err
	}
	var item ports.RouteRecord
	var enabled int
	if err := tx.QueryRowContext(ctx, "SELECT alias, strategy, enabled, created_at FROM routes WHERE alias = ?", alias).Scan(&item.Alias, &item.Strategy, &enabled, &item.CreatedAt); err != nil {
		return ports.RouteRecord{}, err
	}
	item.Enabled = enabled != 0
	rows, err := tx.QueryContext(ctx, "SELECT position, channel, model, weight FROM route_targets WHERE alias = ? ORDER BY position", alias)
	if err != nil {
		return ports.RouteRecord{}, err
	}
	for rows.Next() {
		var target ports.RouteTarget
		if err := rows.Scan(&target.Position, &target.Channel, &target.Model, &target.Weight); err != nil {
			rows.Close()
			return ports.RouteRecord{}, err
		}
		item.Targets = append(item.Targets, target)
	}
	if err := rows.Close(); err != nil {
		return ports.RouteRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.RouteRecord{}, err
	}
	committed = true
	return item, nil
}

func (d *DB) DeleteRoute(ctx context.Context, alias string, audit ports.AuditEvent) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM routes WHERE alias = ?", alias).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ports.ErrKeyNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM route_targets WHERE alias = ?", alias); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM routes WHERE alias = ?", alias); err != nil {
		return err
	}
	audit.Target = alias
	if audit.Action == "" {
		audit.Action = "delete_route"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (d *DB) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, rows.Err()
}

func (d *DB) PutSettings(ctx context.Context, values map[string]string, audit ports.AuditEvent) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			return err
		}
	}
	audit.Action = "update_settings"
	audit.Detail = strings.Join(sortedKeys(values), ",")
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (d *DB) GetChannelOverride(ctx context.Context, slug string) (ports.ChannelOverride, bool, error) {
	var item ports.ChannelOverride
	var enabled int
	var config string
	err := d.db.QueryRowContext(ctx, `SELECT slug, name, adapter, upstream_base, auth_kind, enabled, config, created_at, updated_at FROM channels WHERE slug = ?`, slug).Scan(&item.Slug, &item.Name, &item.Adapter, &item.UpstreamBase, &item.AuthKind, &enabled, &config, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ChannelOverride{}, false, nil
	}
	if err != nil {
		return ports.ChannelOverride{}, false, err
	}
	item.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(config), &item.Config); err != nil || item.Config == nil {
		item.Config = map[string]any{}
	}
	return item, true, nil
}

func (d *DB) ChannelEnabled(ctx context.Context, slug string) (bool, error) {
	var enabled int
	err := d.db.QueryRowContext(ctx, "SELECT enabled FROM channels WHERE slug = ?", slug).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return enabled != 0, nil
}

func (d *DB) UpsertChannelOverride(ctx context.Context, item ports.ChannelOverride, audit ports.AuditEvent) (ports.ChannelOverride, error) {
	if item.CreatedAt == 0 {
		item.CreatedAt = time.Now().Unix()
	}
	item.UpdatedAt = time.Now().Unix()
	if item.Config == nil {
		item.Config = map[string]any{}
	}
	encoded, err := json.Marshal(item.Config)
	if err != nil {
		return ports.ChannelOverride{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.ChannelOverride{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.ExecContext(ctx, `INSERT INTO channels(slug,name,adapter,upstream_base,auth_kind,enabled,config,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(slug) DO UPDATE SET name=excluded.name, adapter=excluded.adapter,
		upstream_base=excluded.upstream_base, auth_kind=excluded.auth_kind, enabled=excluded.enabled,
		config=excluded.config, updated_at=excluded.updated_at`, item.Slug, item.Name, item.Adapter, item.UpstreamBase, item.AuthKind, boolInt(item.Enabled), string(encoded), item.CreatedAt, item.UpdatedAt)
	if err != nil {
		return ports.ChannelOverride{}, err
	}
	audit.Target = item.Slug
	if audit.Action == "" {
		audit.Action = "update_channel_config"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.ChannelOverride{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.ChannelOverride{}, err
	}
	committed = true
	return item, nil
}

func (d *DB) DeleteChannelOverride(ctx context.Context, slug string, audit ports.AuditEvent) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM channels WHERE slug = ?", slug).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ports.ErrKeyNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM channels WHERE slug = ?", slug); err != nil {
		return err
	}
	audit.Target = slug
	if audit.Action == "" {
		audit.Action = "delete_channel_config"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (d *DB) ListUsers(ctx context.Context) ([]ports.UserRecord, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT username, role, enabled, created_at, updated_at FROM users ORDER BY username")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	result := make([]ports.UserRecord, 0)
	for rows.Next() {
		var item ports.UserRecord
		var enabled int
		if err := rows.Scan(&item.Username, &item.Role, &enabled, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		result = append(result, item)
	}
	return result, rows.Err()
}

func (d *DB) CreateUser(ctx context.Context, user ports.UserRecord, audit ports.AuditEvent) (ports.UserRecord, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.UserRecord{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(username, role, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?)", user.Username, user.Role, boolInt(user.Enabled), user.CreatedAt, user.UpdatedAt); err != nil {
		return ports.UserRecord{}, fmt.Errorf("create user: %w", err)
	}
	audit.Target = user.Username
	if audit.Action == "" {
		audit.Action = "create_user"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.UserRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.UserRecord{}, err
	}
	committed = true
	return user, nil
}

func (d *DB) UpdateUser(ctx context.Context, username string, input ports.UserRecord, audit ports.AuditEvent) (ports.UserRecord, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.UserRecord{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var current ports.UserRecord
	var enabled int
	if err := tx.QueryRowContext(ctx, "SELECT username, role, enabled, created_at, updated_at FROM users WHERE username = ?", username).Scan(&current.Username, &current.Role, &enabled, &current.CreatedAt, &current.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return ports.UserRecord{}, ports.ErrKeyNotFound
	} else if err != nil {
		return ports.UserRecord{}, err
	}
	current.Enabled = enabled != 0
	if input.Role != "" {
		current.Role = input.Role
	}
	current.Enabled = input.Enabled
	current.UpdatedAt = time.Now().Unix()
	if _, err := tx.ExecContext(ctx, "UPDATE users SET role = ?, enabled = ?, updated_at = ? WHERE username = ?", current.Role, boolInt(current.Enabled), current.UpdatedAt, username); err != nil {
		return ports.UserRecord{}, err
	}
	audit.Target = username
	if audit.Action == "" {
		audit.Action = "update_user"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.UserRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.UserRecord{}, err
	}
	committed = true
	return current, nil
}

func (d *DB) DeleteUser(ctx context.Context, username string, audit ports.AuditEvent) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.ExecContext(ctx, "DELETE FROM users WHERE username = ?", username)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return ports.ErrKeyNotFound
	}
	audit.Target = username
	if audit.Action == "" {
		audit.Action = "delete_user"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (d *DB) SystemInfo(ctx context.Context, statePath string, channels []ports.SystemChannel) (ports.SystemInfo, error) {
	var count int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		return ports.SystemInfo{}, err
	}
	info := ports.SystemInfo{Service: "all2api-api", Version: "0.1.0", GoVersion: runtime.Version(), Platform: runtime.GOOS, Implementation: "go", SchemaVersion: SchemaVersion, Channels: channels}
	if stat, err := os.Stat(d.path); err == nil {
		info.Database = map[string]any{"present": true, "bytes": stat.Size()}
	} else {
		info.Database = map[string]any{"present": false, "bytes": int64(0)}
	}
	info.RuntimeState = runtimeFileStatus(statePath)
	return info, nil
}

func (d *DB) StorageHealth(ctx context.Context, statePath string) (ports.StorageHealth, error) {
	health := ports.StorageHealth{Status: "ok", Database: map[string]any{"status": "ok", "bytes": int64(0)}, RuntimeState: runtimeFileStatus(statePath), Disk: map[string]any{"status": "ok"}}
	if stat, err := os.Stat(d.path); err == nil {
		health.Database["bytes"] = stat.Size()
	} else {
		health.Database["status"] = "missing"
		health.Status = "degraded"
	}
	if health.RuntimeState["status"] == "invalid" || health.RuntimeState["status"] == "unreadable" {
		health.Status = "degraded"
	}
	if stat, err := os.Stat(filepath.Dir(d.path)); err == nil {
		_ = stat
		if usage, err := diskUsage(filepath.Dir(d.path)); err == nil {
			health.Disk["total_bytes"] = usage.total
			health.Disk["free_bytes"] = usage.free
			health.Disk["used_bytes"] = usage.used
		}
	} else {
		health.Disk["status"] = "error"
		health.Status = "degraded"
	}
	if err := d.db.PingContext(ctx); err != nil {
		health.Database["status"] = "error"
		health.Status = "degraded"
	}
	return health, nil
}

func (d *DB) Metrics(ctx context.Context, days int) (ports.SystemMetrics, error) {
	start := time.Now().UTC().AddDate(0, 0, -(days - 1)).Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, days)
	rows, err := d.db.QueryContext(ctx, "SELECT channel, status, latency_ms, stream FROM request_logs WHERE ts >= ? AND ts < ?", start.Unix(), end.Unix())
	if err != nil {
		return ports.SystemMetrics{}, err
	}
	defer rows.Close()
	metric := ports.SystemMetrics{From: start.Format("2006-01-02"), To: end.Add(-time.Second).Format("2006-01-02"), Channels: []ports.ChannelMetric{}, Accounts: map[string]int64{}}
	latencies := make([]int64, 0)
	byChannel := map[string]*ports.ChannelMetric{}
	for rows.Next() {
		var channel string
		var status, stream int
		var latency int64
		if err := rows.Scan(&channel, &status, &latency, &stream); err != nil {
			return ports.SystemMetrics{}, err
		}
		metric.Requests++
		if status >= 400 {
			metric.Errors++
		}
		if stream != 0 {
			metric.Streams++
		}
		if latency < 0 {
			latency = 0
		}
		latencies = append(latencies, latency)
		item := byChannel[channel]
		if item == nil {
			item = &ports.ChannelMetric{Channel: channel}
			byChannel[channel] = item
		}
		item.Requests++
		item.AvgLatencyMS += float64(latency)
		if status >= 400 {
			item.Errors++
		}
	}
	if metric.Requests > 0 {
		metric.ErrorRate = float64(metric.Errors) / float64(metric.Requests)
		for _, value := range latencies {
			metric.AvgLatencyMS += float64(value)
		}
		metric.AvgLatencyMS /= float64(metric.Requests)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		index := int(float64(len(latencies)-1) * 0.95)
		metric.P95LatencyMS = latencies[index]
	}
	for _, item := range byChannel {
		if item.Requests > 0 {
			item.ErrorRate = float64(item.Errors) / float64(item.Requests)
			item.AvgLatencyMS /= float64(item.Requests)
		}
		metric.Channels = append(metric.Channels, *item)
	}
	sort.Slice(metric.Channels, func(i, j int) bool { return metric.Channels[i].Channel < metric.Channels[j].Channel })
	var totalAccounts, enabledAccounts, observedAccounts int64
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM accounts").Scan(&totalAccounts, &enabledAccounts); err != nil {
		return ports.SystemMetrics{}, err
	}
	metric.Accounts["total"] = totalAccounts
	metric.Accounts["enabled"] = enabledAccounts
	metric.Accounts["available"] = enabledAccounts
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_runtime_state").Scan(&observedAccounts); err != nil {
		return ports.SystemMetrics{}, err
	}
	metric.Accounts["runtime_observed"] = observedAccounts
	return metric, nil
}

type diskStat struct{ total, free, used uint64 }

func diskUsage(path string) (diskStat, error) {
	// Windows uses a small PowerShell-free approximation here; exact disk
	// telemetry remains an infrastructure concern and is not used for gating.
	info, err := os.Stat(path)
	if err != nil {
		return diskStat{}, err
	}
	_ = info
	return diskStat{}, nil
}

func runtimeFileStatus(path string) map[string]any {
	if path == "" {
		return map[string]any{"status": "not_initialized", "present": false}
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]any{"status": "not_initialized", "present": false}
		}
		return map[string]any{"status": "unreadable", "present": true, "error": "stat_failed"}
	}
	return map[string]any{"status": "ok", "present": true}
}

func sortedKeys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func scanModel(row rowScanner) (ports.ModelRecord, error) {
	var item ports.ModelRecord
	var caps string
	var enabled int
	var contextWindow, maxOutput sql.NullInt64
	if err := row.Scan(&item.ID, &item.Channel, &item.UpstreamID, &item.DisplayName, &item.Kind, &caps, &contextWindow, &maxOutput, &item.Multiplier, &enabled); err != nil {
		return ports.ModelRecord{}, err
	}
	if err := json.Unmarshal([]byte(caps), &item.Capabilities); err != nil {
		return ports.ModelRecord{}, fmt.Errorf("decode model capabilities: %w", err)
	}
	if contextWindow.Valid {
		value := contextWindow.Int64
		item.ContextWindow = &value
	}
	if maxOutput.Valid {
		value := maxOutput.Int64
		item.MaxOutput = &value
	}
	item.Enabled = enabled != 0
	return item, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (d *DB) ListKeys(ctx context.Context, filter ports.KeyListFilter) ([]ports.APIKey, int, error) {
	page := filter.Page
	if page < 1 {
		page = 1
	}
	pageSize := filter.PageSize
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	where := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if filter.Search != "" {
		search := escapeLike(filter.Search)
		where = append(where, `(name LIKE ? ESCAPE '\' OR prefix LIKE ? ESCAPE '')`)
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	if filter.Enabled != nil {
		where = append(where, "enabled = ?")
		if *filter.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_keys"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count API keys: %w", err)
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := d.db.QueryContext(ctx, `SELECT id, name, prefix, enabled, expires_at, channels,
		models, limit_rpm, created_at, last_used_at, key_encrypted
		FROM api_keys`+whereSQL+" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list API keys: %w", err)
	}
	defer rows.Close()
	result := make([]ports.APIKey, 0)
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, key)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate API keys: %w", err)
	}
	return result, total, nil
}

func (d *DB) GetKey(ctx context.Context, id int64) (ports.APIKey, error) {
	row := d.db.QueryRowContext(ctx, `SELECT id, name, prefix, enabled, expires_at, channels,
		models, limit_rpm, created_at, last_used_at, key_encrypted FROM api_keys WHERE id = ?`, id)
	key, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.APIKey{}, ports.ErrKeyNotFound
	}
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("get API key: %w", err)
	}
	return key, nil
}

func (d *DB) CreateKey(ctx context.Context, input ports.KeyCreate) (ports.APIKey, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("begin key create: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM api_keys WHERE name = ? COLLATE NOCASE LIMIT 1", input.Name).Scan(&exists); err == nil {
		return ports.APIKey{}, ports.ErrKeyNameExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ports.APIKey{}, fmt.Errorf("check API key name: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO api_keys
		(name, key_hash, key_encrypted, prefix, enabled, expires_at, channels, models, limit_rpm, created_at)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		input.Name, input.KeyHash, input.Encrypted, input.Prefix, input.ExpiresAt,
		jsonString(input.Channels), jsonString(input.Models), input.LimitRPM, input.CreatedAt)
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("insert API key: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("read API key id: %w", err)
	}
	if err := insertAuditTx(ctx, tx, input.Audit, id); err != nil {
		return ports.APIKey{}, err
	}
	key, err := scanAPIKey(tx.QueryRowContext(ctx, `SELECT id, name, prefix, enabled, expires_at, channels,
		models, limit_rpm, created_at, last_used_at, key_encrypted FROM api_keys WHERE id = ?`, id))
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("read created API key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ports.APIKey{}, fmt.Errorf("commit key create: %w", err)
	}
	committed = true
	return key, nil
}

func (d *DB) UpdateKey(ctx context.Context, id int64, input ports.KeyPatch) (ports.APIKey, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("begin key update: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var currentName string
	if err := tx.QueryRowContext(ctx, "SELECT name FROM api_keys WHERE id = ?", id).Scan(&currentName); errors.Is(err, sql.ErrNoRows) {
		return ports.APIKey{}, ports.ErrKeyNotFound
	} else if err != nil {
		return ports.APIKey{}, fmt.Errorf("read API key before update: %w", err)
	}
	sets := make([]string, 0, 6)
	values := make([]any, 0, 7)
	if input.Name != nil {
		var duplicate int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM api_keys WHERE name = ? COLLATE NOCASE AND id != ? LIMIT 1", *input.Name, id).Scan(&duplicate); err == nil {
			return ports.APIKey{}, ports.ErrKeyNameExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return ports.APIKey{}, fmt.Errorf("check updated API key name: %w", err)
		}
		sets = append(sets, "name = ?")
		values = append(values, *input.Name)
	}
	if input.Enabled != nil {
		sets = append(sets, "enabled = ?")
		if *input.Enabled {
			values = append(values, 1)
		} else {
			values = append(values, 0)
		}
	}
	if input.Channels != nil {
		sets = append(sets, "channels = ?")
		values = append(values, jsonString(*input.Channels))
	}
	if input.Models != nil {
		sets = append(sets, "models = ?")
		values = append(values, jsonString(*input.Models))
	}
	if input.ExpiresAtSet {
		sets = append(sets, "expires_at = ?")
		values = append(values, input.ExpiresAt)
	}
	if input.LimitRPM != nil {
		sets = append(sets, "limit_rpm = ?")
		values = append(values, *input.LimitRPM)
	}
	if len(sets) == 0 {
		return ports.APIKey{}, fmt.Errorf("no key fields were provided")
	}
	values = append(values, id)
	if _, err := tx.ExecContext(ctx, "UPDATE api_keys SET "+strings.Join(sets, ", ")+" WHERE id = ?", values...); err != nil {
		return ports.APIKey{}, fmt.Errorf("update API key: %w", err)
	}
	detail := input.Audit.Detail
	if detail == "" {
		detail = currentName
	}
	audit := input.Audit
	audit.Detail = detail
	if err := insertAuditTx(ctx, tx, audit, id); err != nil {
		return ports.APIKey{}, err
	}
	key, err := scanAPIKey(tx.QueryRowContext(ctx, `SELECT id, name, prefix, enabled, expires_at, channels,
		models, limit_rpm, created_at, last_used_at, key_encrypted FROM api_keys WHERE id = ?`, id))
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("read updated API key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ports.APIKey{}, fmt.Errorf("commit key update: %w", err)
	}
	committed = true
	return key, nil
}

func (d *DB) RotateKey(ctx context.Context, id int64, keyHash, encrypted, prefix string, audit ports.AuditEvent) (ports.APIKey, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("begin key rotation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var name string
	if err := tx.QueryRowContext(ctx, "SELECT name FROM api_keys WHERE id = ?", id).Scan(&name); errors.Is(err, sql.ErrNoRows) {
		return ports.APIKey{}, ports.ErrKeyNotFound
	} else if err != nil {
		return ports.APIKey{}, fmt.Errorf("read API key before rotation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE api_keys SET key_hash = ?, key_encrypted = ?, prefix = ?, enabled = 1 WHERE id = ?", keyHash, encrypted, prefix, id); err != nil {
		return ports.APIKey{}, fmt.Errorf("rotate API key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM api_key_rate_events WHERE key_id = ?", id); err != nil {
		return ports.APIKey{}, fmt.Errorf("reset API key rate events: %w", err)
	}
	if audit.Detail == "" {
		audit.Detail = name
	}
	if err := insertAuditTx(ctx, tx, audit, id); err != nil {
		return ports.APIKey{}, err
	}
	key, err := scanAPIKey(tx.QueryRowContext(ctx, `SELECT id, name, prefix, enabled, expires_at, channels,
		models, limit_rpm, created_at, last_used_at, key_encrypted FROM api_keys WHERE id = ?`, id))
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("read rotated API key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ports.APIKey{}, fmt.Errorf("commit key rotation: %w", err)
	}
	committed = true
	return key, nil
}

func (d *DB) RevokeKey(ctx context.Context, id int64, audit ports.AuditEvent) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin key revocation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var name string
	if err := tx.QueryRowContext(ctx, "SELECT name FROM api_keys WHERE id = ?", id).Scan(&name); errors.Is(err, sql.ErrNoRows) {
		return ports.ErrKeyNotFound
	} else if err != nil {
		return fmt.Errorf("read API key before revocation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE api_keys SET enabled = 0 WHERE id = ?", id); err != nil {
		return fmt.Errorf("revoke API key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM api_key_rate_events WHERE key_id = ?", id); err != nil {
		return fmt.Errorf("clear API key rate events: %w", err)
	}
	if audit.Detail == "" {
		audit.Detail = name
	}
	if err := insertAuditTx(ctx, tx, audit, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit key revocation: %w", err)
	}
	committed = true
	return nil
}

func (d *DB) AuthenticateKey(ctx context.Context, hash string, now time.Time) (ports.APIKey, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("begin API key authentication: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	key, err := scanAPIKey(tx.QueryRowContext(ctx, `SELECT id, name, prefix, enabled, expires_at, channels,
		models, limit_rpm, created_at, last_used_at, key_encrypted FROM api_keys
		WHERE key_hash = ? AND enabled = 1`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return ports.APIKey{}, ports.ErrInvalidKey
	}
	if err != nil {
		return ports.APIKey{}, fmt.Errorf("find API key: %w", err)
	}
	if key.ExpiresAt != nil && *key.ExpiresAt <= now.Unix() {
		return ports.APIKey{}, ports.ErrInvalidKey
	}
	if key.LimitRPM > 0 {
		cutoff := now.Unix() - 60
		if _, err := tx.ExecContext(ctx, "DELETE FROM api_key_rate_events WHERE key_id = ? AND ts < ?", key.ID, cutoff); err != nil {
			return ports.APIKey{}, fmt.Errorf("prune API key rate events: %w", err)
		}
		var recent int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_key_rate_events WHERE key_id = ? AND ts >= ?", key.ID, cutoff).Scan(&recent); err != nil {
			return ports.APIKey{}, fmt.Errorf("count API key rate events: %w", err)
		}
		if recent >= key.LimitRPM {
			return ports.APIKey{}, ports.ErrKeyRateLimited
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO api_key_rate_events(key_id, ts) VALUES (?, ?)", key.ID, now.Unix()); err != nil {
			return ports.APIKey{}, fmt.Errorf("record API key rate event: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE id = ?", now.Unix(), key.ID); err != nil {
		return ports.APIKey{}, fmt.Errorf("update API key usage: %w", err)
	}
	usedAt := now.Unix()
	key.LastUsedAt = &usedAt
	if err := tx.Commit(); err != nil {
		return ports.APIKey{}, fmt.Errorf("commit API key authentication: %w", err)
	}
	committed = true
	return key, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanAPIKey(row rowScanner) (ports.APIKey, error) {
	var key ports.APIKey
	var enabled int
	var expiresAt, lastUsedAt sql.NullInt64
	var channelsJSON, modelsJSON string
	var encrypted sql.NullString
	if err := row.Scan(&key.ID, &key.Name, &key.Prefix, &enabled, &expiresAt, &channelsJSON, &modelsJSON, &key.LimitRPM, &key.CreatedAt, &lastUsedAt, &encrypted); err != nil {
		return ports.APIKey{}, err
	}
	key.Enabled = enabled != 0
	if expiresAt.Valid {
		value := expiresAt.Int64
		key.ExpiresAt = &value
	}
	if lastUsedAt.Valid {
		value := lastUsedAt.Int64
		key.LastUsedAt = &value
	}
	if err := json.Unmarshal([]byte(channelsJSON), &key.Channels); err != nil {
		return ports.APIKey{}, fmt.Errorf("decode API key channels: %w", err)
	}
	if err := json.Unmarshal([]byte(modelsJSON), &key.Models); err != nil {
		return ports.APIKey{}, fmt.Errorf("decode API key models: %w", err)
	}
	key.HasEncryptedKey = encrypted.Valid && encrypted.String != ""
	return key, nil
}

func insertAuditTx(ctx context.Context, tx *sql.Tx, event ports.AuditEvent, targetID int64) error {
	target := event.Target
	if target == "" {
		target = fmt.Sprintf("%d", targetID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(ts, actor, action, target, detail, ip)
		VALUES (unixepoch(), ?, ?, ?, ?, ?)`, event.Actor, event.Action, target, event.Detail, event.IP); err != nil {
		return fmt.Errorf("record key audit event: %w", err)
	}
	return nil
}

func jsonString(values []string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func nullableStringValue(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "%", `\%`)
	return strings.ReplaceAll(value, "_", `\_`)
}

func ensureParent(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	return nil
}
