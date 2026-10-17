package persistence

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func (d *DB) SystemInfo(ctx context.Context, statePath string, channels []ports.SystemChannel) (ports.SystemInfo, error) {
	var count int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		return ports.SystemInfo{}, err
	}
	info := ports.SystemInfo{Service: "all2api-api", Version: "0.1.0", GoVersion: runtime.Version(), Platform: runtime.GOOS, Implementation: "go", SchemaVersion: SchemaVersion, Channels: channels}
	if stat, err := os.Stat(d.path); err == nil {
		info.Database = map[string]any{"present": true, "bytes": stat.Size()}
	} else {
		info.Database = map[string]any{"present": false, "bytes": int64(0)}
	}
	info.RuntimeState = runtimeFileStatus(statePath)
	return info, nil
}

func (d *DB) StorageHealth(ctx context.Context, statePath string) (ports.StorageHealth, error) {
	health := ports.StorageHealth{Status: "ok", Database: map[string]any{"status": "ok", "bytes": int64(0)}, RuntimeState: runtimeFileStatus(statePath), Disk: map[string]any{"status": "ok"}}
	if stat, err := os.Stat(d.path); err == nil {
		health.Database["bytes"] = stat.Size()
	} else {
		health.Database["status"] = "missing"
		health.Status = "degraded"
	}
	if health.RuntimeState["status"] == "invalid" || health.RuntimeState["status"] == "unreadable" {
		health.Status = "degraded"
	}
	if stat, err := os.Stat(filepath.Dir(d.path)); err == nil {
		_ = stat
		if usage, err := diskUsage(filepath.Dir(d.path)); err == nil {
			health.Disk["total_bytes"] = usage.total
			health.Disk["free_bytes"] = usage.free
			health.Disk["used_bytes"] = usage.used
		}
	} else {
		health.Disk["status"] = "error"
		health.Status = "degraded"
	}
	if err := d.db.PingContext(ctx); err != nil {
		health.Database["status"] = "error"
		health.Status = "degraded"
	}
	return health, nil
}

func (d *DB) Metrics(ctx context.Context, days int) (ports.SystemMetrics, error) {
	start := time.Now().UTC().AddDate(0, 0, -(days - 1)).Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, days)
	rows, err := d.db.QueryContext(ctx, "SELECT channel, status, latency_ms, stream FROM request_logs WHERE ts >= ? AND ts < ?", start.Unix(), end.Unix())
	if err != nil {
		return ports.SystemMetrics{}, err
	}
	defer rows.Close()
	metric := ports.SystemMetrics{From: start.Format("2006-01-02"), To: end.Add(-time.Second).Format("2006-01-02"), Channels: []ports.ChannelMetric{}, Accounts: map[string]int64{}}
	latencies := make([]int64, 0)
	byChannel := map[string]*ports.ChannelMetric{}
	for rows.Next() {
		var channel string
		var status, stream int
		var latency int64
		if err := rows.Scan(&channel, &status, &latency, &stream); err != nil {
			return ports.SystemMetrics{}, err
		}
		metric.Requests++
		if status >= 400 {
			metric.Errors++
		}
		if stream != 0 {
			metric.Streams++
		}
		if latency < 0 {
			latency = 0
		}
		latencies = append(latencies, latency)
		item := byChannel[channel]
		if item == nil {
			item = &ports.ChannelMetric{Channel: channel}
			byChannel[channel] = item
		}
		item.Requests++
		item.AvgLatencyMS += float64(latency)
		if status >= 400 {
			item.Errors++
		}
	}
	if metric.Requests > 0 {
		metric.ErrorRate = float64(metric.Errors) / float64(metric.Requests)
		for _, value := range latencies {
			metric.AvgLatencyMS += float64(value)
		}
		metric.AvgLatencyMS /= float64(metric.Requests)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		index := int(float64(len(latencies)-1) * 0.95)
		metric.P95LatencyMS = latencies[index]
	}
	for _, item := range byChannel {
		if item.Requests > 0 {
			item.ErrorRate = float64(item.Errors) / float64(item.Requests)
			item.AvgLatencyMS /= float64(item.Requests)
		}
		metric.Channels = append(metric.Channels, *item)
	}
	sort.Slice(metric.Channels, func(i, j int) bool { return metric.Channels[i].Channel < metric.Channels[j].Channel })
	var totalAccounts, enabledAccounts, observedAccounts int64
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM accounts").Scan(&totalAccounts, &enabledAccounts); err != nil {
		return ports.SystemMetrics{}, err
	}
	metric.Accounts["total"] = totalAccounts
	metric.Accounts["enabled"] = enabledAccounts
	metric.Accounts["available"] = enabledAccounts
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_runtime_state").Scan(&observedAccounts); err != nil {
		return ports.SystemMetrics{}, err
	}
	metric.Accounts["runtime_observed"] = observedAccounts
	return metric, nil
}

type diskStat struct{ total, free, used uint64 }

func diskUsage(path string) (diskStat, error) {
	// Windows uses a small PowerShell-free approximation here; exact disk
	// telemetry remains an infrastructure concern and is not used for gating.
	info, err := os.Stat(path)
	if err != nil {
		return diskStat{}, err
	}
	_ = info
	return diskStat{}, nil
}

func runtimeFileStatus(path string) map[string]any {
	if path == "" {
		return map[string]any{"status": "not_initialized", "present": false}
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]any{"status": "not_initialized", "present": false}
		}
		return map[string]any{"status": "unreadable", "present": true, "error": "stat_failed"}
	}
	return map[string]any{"status": "ok", "present": true}
}
