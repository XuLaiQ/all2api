package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

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
	// channel, model and error_kind are nullable in the schema (rows written by
	// earlier versions leave error_kind NULL on success), so read them as ''.
	rows, err := d.db.QueryContext(ctx, `SELECT id, ts, request_id, COALESCE(channel, ''), key_id, COALESCE(model, ''), upstream_model, route_alias, fallback_depth, status,
		COALESCE(error_kind, ''), stream, prompt_tokens, completion_tokens, usage_reported, usage_kind, ttft_ms, latency_ms
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
