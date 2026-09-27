import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { ApiClientError } from "../../api/client";
import { fetchAuditLogs, type AuditFilters, type AuditRecord } from "./auditApi";
import { DateInput } from "../../app/controls/DateInput";

type FilterDraft = { actor: string; action: string; target: string; from: string; to: string };
const emptyFilters: FilterDraft = { actor: "", action: "", target: "", from: "", to: "" };

function toUtcStart(day: string): string | undefined {
  return day ? `${day}T00:00:00Z` : undefined;
}

function toUtcEndExclusive(day: string): string | undefined {
  if (!day) return undefined;
  const date = new Date(`${day}T00:00:00Z`);
  date.setUTCDate(date.getUTCDate() + 1);
  return date.toISOString().replace(".000Z", "Z");
}

function formatTimestamp(value: string): string {
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "medium",
    timeZone: "UTC",
  }).format(new Date(value));
}

export function AuditPage() {
  const [draft, setDraft] = useState<FilterDraft>(emptyFilters);
  const [filters, setFilters] = useState<AuditFilters>({});
  const [page, setPage] = useState(1);
  const [rows, setRows] = useState<AuditRecord[]>([]);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchAuditLogs(page, filters, controller.signal)
      .then((result) => {
        setRows(result.data);
        setTotal(result.pagination.total);
        setTotalPages(result.pagination.total_pages);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof ApiClientError ? cause.message : "读取审计日志失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, filters, retry]);

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next: AuditFilters = {};
    if (draft.actor.trim()) next.actor = draft.actor.trim();
    if (draft.action.trim()) next.action = draft.action.trim();
    if (draft.target.trim()) next.target = draft.target.trim();
    if (draft.from) next.from = toUtcStart(draft.from);
    if (draft.to) next.to = toUtcEndExclusive(draft.to);
    setPage(1);
    setFilters(next);
  }

  function clearFilters() {
    setDraft(emptyFilters);
    setPage(1);
    setFilters({});
  }

  return (
    <main className="page-content data-page audit-page">
      <div className="page-heading">
        <div>
          <h1>审计日志</h1>
          <p>管理操作摘要 · 时间为 UTC · 不显示来源 IP</p>
        </div>
      </div>

      <form className="log-filter-form" onSubmit={applyFilters}>
        <label><span>操作者</span><input value={draft.actor} onChange={(event) => setDraft({ ...draft, actor: event.target.value })} placeholder="精确匹配" /></label>
        <label><span>操作</span><input value={draft.action} onChange={(event) => setDraft({ ...draft, action: event.target.value })} placeholder="精确匹配" /></label>
        <label><span>目标</span><input value={draft.target} onChange={(event) => setDraft({ ...draft, target: event.target.value })} placeholder="精确匹配" /></label>
        <label><span>从日期</span><DateInput value={draft.from} onChange={(from) => setDraft({ ...draft, from })} /></label>
        <label><span>到日期</span><DateInput value={draft.to} onChange={(to) => setDraft({ ...draft, to })} /></label>
        <div className="log-filter-actions">
          <button type="submit">筛选</button>
          <button type="button" className="secondary-action" onClick={clearFilters}>清除</button>
        </div>
      </form>

      {error && <div className="notice notice-error" role="alert"><span>{error}</span><button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button></div>}

      {loading ? (
        <div className="table-state" aria-live="polite">正在读取审计日志…</div>
      ) : rows.length === 0 ? (
        <div className="table-state">没有符合条件的审计记录</div>
      ) : (
        <div className="table-wrap">
          <table className="data-table audit-table">
            <thead><tr><th>时间 (UTC)</th><th>操作者</th><th>操作</th><th>目标</th><th>摘要</th></tr></thead>
            <tbody>{rows.map((row) => <AuditRow key={row.id} row={row} />)}</tbody>
          </table>
        </div>
      )}

      <div className="log-pagination">
        <span>第 {page} / {Math.max(totalPages, 1)} 页 · 共 {new Intl.NumberFormat("zh-CN").format(total)} 条</span>
        <div>
          <button type="button" disabled={loading || page <= 1} onClick={() => setPage((value) => value - 1)}>上一页</button>
          <button type="button" disabled={loading || page >= totalPages} onClick={() => setPage((value) => value + 1)}>下一页</button>
        </div>
      </div>
    </main>
  );
}

function AuditRow({ row }: { row: AuditRecord }) {
  return (
    <tr>
      <td>{formatTimestamp(row.ts)}</td>
      <td>{row.actor || "—"}</td>
      <td><span className="status-label status-info">{row.action}</span></td>
      <td><span className="log-request-id">{row.target || "—"}</span></td>
      <td>{row.detail || "—"}</td>
    </tr>
  );
}
