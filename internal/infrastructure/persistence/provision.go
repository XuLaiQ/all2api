package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

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

// terminalProvisionStatuses are the session states that can never resume.
var terminalProvisionStatuses = []any{"succeeded", "cancelled", "expired", "failed", "failed_terminal"}

// PurgeExpiredProvisionState removes expired idempotency records and expired
// sessions that already reached a terminal state. Active sessions are kept even
// when expired so an in-flight flow can still report its own expiry.
func (d *DB) PurgeExpiredProvisionState(ctx context.Context, cutoff time.Time) (int64, error) {
	at := float64(cutoff.Unix())
	first, err := d.db.ExecContext(ctx, "DELETE FROM provision_idempotency WHERE expires_at IS NOT NULL AND expires_at <= ?", at)
	if err != nil {
		return 0, fmt.Errorf("purge provision idempotency: %w", err)
	}
	args := append([]any{at}, terminalProvisionStatuses...)
	second, err := d.db.ExecContext(ctx, `DELETE FROM provision_sessions
		WHERE expires_at <= ? AND status IN (?, ?, ?, ?, ?)`, args...)
	if err != nil {
		return 0, fmt.Errorf("purge provision sessions: %w", err)
	}
	removedIdempotency, _ := first.RowsAffected()
	removedSessions, _ := second.RowsAffected()
	return removedIdempotency + removedSessions, nil
}
