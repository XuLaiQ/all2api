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

const uncuratedModelNoise = `(
	display_name_locked = 0 AND (
		(lower(channel) = 'chatgpt' AND lower(upstream_id) GLOB 'chatgpt_alpha_model_external_access_reserved_gate_*') OR
		(lower(channel) = 'doubao' AND length(upstream_id) <= 2 AND upstream_id != '' AND upstream_id NOT GLOB '*[^0-9]*' AND trim(display_name) = upstream_id)
	)
)`

func (d *DB) ListModels(ctx context.Context, query ports.ModelQuery) ([]ports.ModelRecord, int, []string, error) {
	where := []string{"NOT " + uncuratedModelNoise}
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
		where = append(where, `(instr(lower(id), lower(?)) > 0 OR instr(lower(upstream_id), lower(?)) > 0 OR instr(lower(public_id), lower(?)) > 0 OR instr(lower(display_name), lower(?)) > 0)`)
		args = append(args, query.Search, query.Search, query.Search, query.Search)
	}
	whereSQL := " WHERE " + strings.Join(where, " AND ")
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
	rows, err := d.db.QueryContext(ctx, `SELECT id, channel, upstream_id, public_id, display_name, kind, caps,
		context_window, max_output, multiplier, enabled, display_name_locked, public_id_locked
		FROM models`+whereSQL+" ORDER BY channel, display_name, id LIMIT ? OFFSET ?", listArgs...)
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
	kindRows, err := d.db.QueryContext(ctx, "SELECT DISTINCT kind FROM models WHERE kind != '' AND NOT "+uncuratedModelNoise+" ORDER BY kind")
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
	existingPublicIDs := make(map[string]string)
	usedPublicIDs := make(map[string]bool)
	existingRows, err := tx.QueryContext(ctx, "SELECT id, public_id FROM models WHERE channel = ?", channel)
	if err != nil {
		return 0, fmt.Errorf("read existing model public ids: %w", err)
	}
	for existingRows.Next() {
		var id, publicID string
		if err := existingRows.Scan(&id, &publicID); err != nil {
			_ = existingRows.Close()
			return 0, fmt.Errorf("scan existing model public ids: %w", err)
		}
		existingPublicIDs[id] = publicID
		if publicID != "" {
			usedPublicIDs[publicID] = true
		}
	}
	if err := existingRows.Err(); err != nil {
		_ = existingRows.Close()
		return 0, fmt.Errorf("iterate existing model public ids: %w", err)
	}
	if err := existingRows.Close(); err != nil {
		return 0, fmt.Errorf("close existing model public ids: %w", err)
	}
	count := 0
	seenIDs := make(map[string]bool, len(models))
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
		if isUncuratedModelNoise(channel, model.UpstreamID, name) {
			continue
		}
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		// New rows get a generated public_id; existing rows keep theirs so
		// client-facing identifiers stay stable across refreshes.
		publicID, exists := existingPublicIDs[id]
		if !exists || publicID == "" {
			seed := slugModelID(name)
			if seed == "" {
				seed = model.UpstreamID
			}
			publicID = allocateModelPublicID(seed, usedPublicIDs)
			existingPublicIDs[id] = publicID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO models(id, channel, upstream_id, public_id, display_name, kind, caps, multiplier, enabled)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1)
			ON CONFLICT(id) DO UPDATE SET channel=excluded.channel, upstream_id=excluded.upstream_id,
			display_name=CASE WHEN models.display_name_locked=1 THEN models.display_name ELSE excluded.display_name END,
			kind=excluded.kind, caps=excluded.caps`,
			id, channel, model.UpstreamID, publicID, name, kind, string(caps)); err != nil {
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

func (d *DB) UpdateModel(ctx context.Context, id string, update ports.ModelUpdate, audit ports.AuditEvent) (ports.ModelRecord, error) {
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
	var channel string
	if err := tx.QueryRowContext(ctx, "SELECT channel FROM models WHERE id = ?", id).Scan(&channel); errors.Is(err, sql.ErrNoRows) {
		return ports.ModelRecord{}, ports.ErrKeyNotFound
	} else if err != nil {
		return ports.ModelRecord{}, err
	}
	if update.PublicID != nil {
		publicID := slugModelID(*update.PublicID)
		if publicID == "" {
			return ports.ModelRecord{}, fmt.Errorf("%w: public id must not be empty", ports.ErrValidation)
		}
		var conflict string
		err := tx.QueryRowContext(ctx, "SELECT id FROM models WHERE channel = ? AND public_id = ? AND id != ?", channel, publicID, id).Scan(&conflict)
		if err == nil {
			return ports.ModelRecord{}, fmt.Errorf("%w: public id %q already used in channel %q", ports.ErrValidation, publicID, channel)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ports.ModelRecord{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE models SET public_id = ?, public_id_locked = 1 WHERE id = ?", publicID, id); err != nil {
			return ports.ModelRecord{}, fmt.Errorf("update model public id: %w", err)
		}
	}
	if update.DisplayName != nil {
		name := strings.TrimSpace(*update.DisplayName)
		if name == "" {
			return ports.ModelRecord{}, fmt.Errorf("%w: display name must not be empty", ports.ErrValidation)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE models SET display_name = ?, display_name_locked = 1 WHERE id = ?", name, id); err != nil {
			return ports.ModelRecord{}, fmt.Errorf("update model display name: %w", err)
		}
	}
	if update.Enabled != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE models SET enabled = ? WHERE id = ?", boolInt(*update.Enabled), id); err != nil {
			return ports.ModelRecord{}, fmt.Errorf("update model: %w", err)
		}
	}
	audit.Target = id
	if audit.Action == "" {
		audit.Action = "update_model"
	}
	if err := insertAuditTx(ctx, tx, audit, 0); err != nil {
		return ports.ModelRecord{}, err
	}
	item, err := scanModel(tx.QueryRowContext(ctx, modelSelectColumns+` FROM models WHERE id = ?`, id))
	if err != nil {
		return ports.ModelRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ports.ModelRecord{}, err
	}
	committed = true
	return item, nil
}

func (d *DB) ListModelMappings(ctx context.Context) ([]ports.ModelMappingRow, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT channel, public_id, upstream_id FROM models WHERE enabled = 1 AND public_id != '' AND NOT "+uncuratedModelNoise)
	if err != nil {
		return nil, fmt.Errorf("list model mappings: %w", err)
	}
	defer rows.Close()
	result := make([]ports.ModelMappingRow, 0)
	for rows.Next() {
		var item ports.ModelMappingRow
		if err := rows.Scan(&item.Channel, &item.PublicID, &item.UpstreamID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func isUncuratedModelNoise(channel, upstreamID, displayName string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	upstreamID = strings.TrimSpace(upstreamID)
	displayName = strings.TrimSpace(displayName)
	if channel == "chatgpt" && strings.HasPrefix(strings.ToLower(upstreamID), "chatgpt_alpha_model_external_access_reserved_gate_") {
		return true
	}
	if channel != "doubao" || len(upstreamID) == 0 || len(upstreamID) > 2 || displayName != upstreamID {
		return false
	}
	for _, char := range upstreamID {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
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

const modelSelectColumns = `SELECT id, channel, upstream_id, public_id, display_name, kind, caps,
	context_window, max_output, multiplier, enabled, display_name_locked, public_id_locked`

func scanModel(row rowScanner) (ports.ModelRecord, error) {
	var item ports.ModelRecord
	var caps string
	var enabled, displayNameLocked, publicIDLocked int
	var contextWindow, maxOutput sql.NullInt64
	if err := row.Scan(&item.ID, &item.Channel, &item.UpstreamID, &item.PublicID, &item.DisplayName, &item.Kind, &caps,
		&contextWindow, &maxOutput, &item.Multiplier, &enabled, &displayNameLocked, &publicIDLocked); err != nil {
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
	item.DisplayNameLocked = displayNameLocked != 0
	item.PublicIDLocked = publicIDLocked != 0
	return item, nil
}
