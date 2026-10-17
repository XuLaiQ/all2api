package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/XuLaiQ/all2api/internal/ports"
)

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
