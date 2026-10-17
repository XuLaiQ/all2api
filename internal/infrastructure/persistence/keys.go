package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

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

var _ ports.BootstrapKeyRepository = (*DB)(nil)

// UpsertBootstrapKey creates or replaces the configuration-owned bootstrap key.
// Scope is always reset to all channels/models with no expiry.
func (d *DB) UpsertBootstrapKey(ctx context.Context, keyHash, encrypted, prefix string, limitRPM int, now int64) error {
	_, err := d.db.ExecContext(ctx, `INSERT INTO api_keys(name, key_hash, key_encrypted, prefix, created_at, limit_rpm)
		VALUES ('bootstrap', ?, ?, ?, ?, ?)
		ON CONFLICT(name) WHERE name = 'bootstrap' DO UPDATE SET
			key_hash=excluded.key_hash, key_encrypted=excluded.key_encrypted,
			prefix=excluded.prefix, enabled=1, expires_at=NULL,
			channels='[]', models='["*"]', limit_rpm=excluded.limit_rpm`,
		keyHash, encrypted, prefix, now, limitRPM)
	if err != nil {
		return fmt.Errorf("upsert bootstrap key: %w", err)
	}
	return nil
}

// DisableBootstrapKey disables a previously configured bootstrap key so that
// removing A2A_BOOTSTRAP_API_KEY from configuration revokes it.
func (d *DB) DisableBootstrapKey(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, "UPDATE api_keys SET enabled = 0 WHERE name = 'bootstrap'"); err != nil {
		return fmt.Errorf("disable bootstrap key: %w", err)
	}
	return nil
}
