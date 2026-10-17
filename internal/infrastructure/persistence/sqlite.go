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
	"sort"
	"strings"

	"github.com/XuLaiQ/all2api/internal/ports"
	_ "modernc.org/sqlite"
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

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

type rowScanner interface {
	Scan(...any) error
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
