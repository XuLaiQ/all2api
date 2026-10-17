package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

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
