package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

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
