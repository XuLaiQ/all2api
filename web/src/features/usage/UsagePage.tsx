import { useEffect, useMemo, useState } from "react";
import { ApiClientError } from "../../api/client";
import { Select } from "../../app/controls/Select";
import {
  fetchUsageRows,
  fetchUsageSummary,
  type UsageGroup,
  type UsageRow,
  type UsageSummary,
} from "./usageApi";

const groups: { id: UsageGroup; label: string }[] = [
  { id: "daily", label: "按日" },
  { id: "channel", label: "按渠道" },
  { id: "model", label: "按模型" },
  { id: "key", label: "按 Key" },
];

function number(value: number): string {
  return new Intl.NumberFormat("zh-CN").format(value);
}

function dayLabel(value: string): string {
  return new Intl.DateTimeFormat("zh-CN", {
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  }).format(new Date(`${value}T00:00:00Z`));
}

export function UsagePage() {
  const [days, setDays] = useState(30);
  const [group, setGroup] = useState<UsageGroup>("daily");
  const [summary, setSummary] = useState<UsageSummary | null>(null);
  const [rows, setRows] = useState<UsageRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    Promise.all([
      fetchUsageSummary(days, controller.signal),
      fetchUsageRows(group, days, controller.signal),
    ])
      .then(([nextSummary, nextRows]) => {
        setSummary(nextSummary);
        setRows(nextRows);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(cause instanceof ApiClientError ? cause.message : "读取用量统计失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [days, group, retry]);

  const maxRequests = useMemo(
    () => Math.max(1, ...rows.map((row) => row.requests)),
    [rows],
  );

  return (
    <main className="page-content data-page usage-page">
      <div className="page-heading">
        <div>
          <h1>用量统计</h1>
          <p>{summary ? `${summary.from} 至 ${summary.to} · UTC` : "UTC 记账日"}</p>
        </div>
        <label className="range-control">
          <span>统计范围</span>
          <Select
            value={String(days)}
            onChange={(value) => setDays(Number(value))}
            ariaLabel="统计范围"
            options={[
              { value: "7", label: "最近 7 天" },
              { value: "30", label: "最近 30 天" },
              { value: "90", label: "最近 90 天" },
              { value: "366", label: "最近 366 天" },
            ]}
          />
        </label>
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button>
        </div>
      )}

      <section className="metric-grid" aria-label="统计汇总">
        <Metric label="请求数" value={loading ? "…" : number(summary?.requests ?? 0)} />
        <Metric label="已报告 Token" value={loading ? "…" : number(summary?.tokens ?? 0)} />
        <Metric label="Prompt Token" value={loading ? "…" : number(summary?.prompt_tokens ?? 0)} />
        <Metric label="Completion Token" value={loading ? "…" : number(summary?.completion_tokens ?? 0)} />
      </section>

      <div className="usage-status-row">
        <span className="status-label status-success">
          用量已报告 {number(summary?.usage_reported_requests ?? 0)} 次
        </span>
        <span className="status-label status-warning">
          用量未知 {number(summary?.usage_unknown_requests ?? 0)} 次
        </span>
        <span className="credits-unavailable">Credits 暂不可用</span>
      </div>

      <section
        className="data-section surface-panel usage-breakdown"
        id="usage-panel"
        role="tabpanel"
        aria-labelledby={`usage-tab-${group}`}
        tabIndex={0}
      >
        <div className="section-heading section-heading-tabs">
          <div>
            <h2 id="usage-breakdown-title">用量明细</h2>
            <p>请求按最终记账日期、渠道和模型归集</p>
          </div>
          <div className="segmented-tabs" role="tablist" aria-label="用量分组">
            {groups.map((item) => (
              <button
                key={item.id}
                type="button"
                role="tab"
                id={`usage-tab-${item.id}`}
                aria-controls="usage-panel"
                aria-selected={group === item.id}
                className={group === item.id ? "active" : ""}
                onClick={() => setGroup(item.id)}
              >
                {item.label}
              </button>
            ))}
          </div>
        </div>

        {loading ? (
          <div className="table-state" aria-live="polite">正在读取用量…</div>
        ) : rows.length === 0 ? (
          <div className="table-state">所选范围内没有用量记录</div>
        ) : (
          <div className="table-wrap">
            <table className="data-table usage-table">
              <thead><UsageHeader group={group} /></thead>
              <tbody>
                {rows.map((row) => (
                  <UsageTableRow
                    key={rowKey(row, group)}
                    row={row}
                    group={group}
                    maxRequests={maxRequests}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </main>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="metric-tile">
      <span className="metric-label">{label}</span>
      <strong className="metric-value">{value}</strong>
    </div>
  );
}

function UsageHeader({ group }: { group: UsageGroup }) {
  return (
    <tr>
      <th>{group === "daily" ? "日期" : group === "channel" ? "渠道" : group === "model" ? "渠道 / 模型" : "Key"}</th>
      {group === "daily" && <th>请求分布</th>}
      <th>请求</th>
      <th>Prompt</th>
      <th>Completion</th>
      <th>Tokens</th>
      <th>已报告 / 未知</th>
    </tr>
  );
}

function UsageTableRow({
  row,
  group,
  maxRequests,
}: {
  row: UsageRow;
  group: UsageGroup;
  maxRequests: number;
}) {
  let label = "";
  let detail = "";
  if (group === "daily") {
    label = row.day ? dayLabel(row.day) : "—";
    detail = row.day ?? "";
  } else if (group === "channel") {
    label = row.channel ?? "—";
  } else if (group === "model") {
    label = row.model ?? "—";
    detail = row.channel ?? "";
  } else {
    label = row.key_name || `Key #${row.key_id ?? "?"}`;
    detail = `ID ${row.key_id ?? "—"}`;
  }

  return (
    <tr>
      <td>
        <span className="channel-name">{label}</span>
        {detail && <span className="secondary-text">{detail}</span>}
      </td>
      {group === "daily" && (
        <td className="usage-bar-cell">
          <span className="usage-bar-track"><span style={{ width: `${(row.requests / maxRequests) * 100}%` }} /></span>
        </td>
      )}
      <td>{number(row.requests)}</td>
      <td>{number(row.prompt_tokens)}</td>
      <td>{number(row.completion_tokens)}</td>
      <td>{number(row.tokens)}</td>
      <td>{number(row.usage_reported_requests)} / {number(row.usage_unknown_requests)}</td>
    </tr>
  );
}

function rowKey(row: UsageRow, group: UsageGroup): string {
  if (group === "daily") return row.day ?? "daily-empty";
  if (group === "channel") return row.channel ?? "channel-empty";
  if (group === "model") return `${row.channel}:${row.model}`;
  return String(row.key_id ?? row.key_name ?? "key-empty");
}
