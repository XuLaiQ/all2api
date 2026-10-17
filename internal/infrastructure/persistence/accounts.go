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
	"github.com/google/uuid"
)

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
		a.expires_at, a.updated_at, COALESCE(r.success_count, 0), COALESCE(r.fail_count, 0), COALESCE(r.consecutive_failures, 0),
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
		payload, _ := json.Marshal(item.Credentials)
		encrypted, err := box.Encrypt(string(payload))
		if err != nil {
			return ports.AccountImportResult{}, err
		}
		now := time.Now().Unix()
		var existingID, existingCredential string
		err = tx.QueryRowContext(ctx, `SELECT a.id, COALESCE(c.encrypted_payload,'') FROM accounts a LEFT JOIN credentials c ON c.channel = a.channel AND c.account_id = a.native_id WHERE a.channel = ? AND a.native_id = ?`, item.Channel, item.NativeID).Scan(&existingID, &existingCredential)
		if err == nil {
			// Re-importing an existing account (re-login, renewed tokens)
			// replaces its credential and clears the failure state; an
			// identical credential is reported as skipped.
			if existingCredential != "" {
				if plaintext, decryptErr := box.Decrypt(existingCredential); decryptErr == nil && plaintext == string(payload) {
					result.Skipped++
					continue
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE credentials SET encrypted_payload = ?, updated_at = ? WHERE channel = ? AND account_id = ?`, encrypted, now, item.Channel, item.NativeID); err != nil {
				return ports.AccountImportResult{}, err
			}
			if existingCredential == "" {
				ref := fmt.Sprintf("cred_%s_%s", item.Channel, uuid.NewString())
				if _, err := tx.ExecContext(ctx, `INSERT INTO credentials(credential_ref,channel,account_id,encrypted_payload,created_at,updated_at) VALUES(?,?,?,?,?,?)`, ref, item.Channel, item.NativeID, encrypted, now, now); err != nil {
					return ports.AccountImportResult{}, err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET status = 'ready', updated_at = ? WHERE channel = ? AND native_id = ?`, now, item.Channel, item.NativeID); err != nil {
				return ports.AccountImportResult{}, err
			}
			result.Updated++
			result.Accounts = append(result.Accounts, ports.AccountRecord{ID: existingID, Channel: item.Channel, NativeID: item.NativeID, Name: item.Name, Kind: item.Kind, Status: "ready", Enabled: true, QuotaUnit: "none", UpdatedAt: now})
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return ports.AccountImportResult{}, err
		}
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
