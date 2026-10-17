package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

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
