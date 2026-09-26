import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import { clearExpiredLogs, fetchLogs, type LogFilters, type RequestLog } from "./logsApi";

type DraftFilters = {
  request_id: string;
  channel: string;
  model: string;
  status: string;
  error_kind: string;
  stream: string;
  from: string;
  to: string;
};

const emptyFilters: DraftFilters = {
  request_id: "",
  channel: "",
  model: "",
  status: "",
  error_kind: "",
  stream: "",
  from: "",
  to: "",
};

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

function statusTone(status: number): string {
  if (status >= 200 && status < 300) return "status-success";
  if (status >= 400) return "status-danger";
  return "status-warning";
}

export function LogsPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canClear = role === "admin";
  const [draft, setDraft] = useState<DraftFilters>(emptyFilters);
  const [filters, setFilters] = useState<LogFilters>({});
  const [page, setPage] = useState(1);
  const [rows, setRows] = useState<RequestLog[]>([]);
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [confirmClear, setConfirmClear] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [clearMessage, setClearMessage] = useState("");
  const [clearError, setClearError] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    fetchChannels(controller.signal)
      .then(setChannels)
      .catch(() => undefined);
    return () => controller.abort();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchLogs(page, filters, controller.signal)
      .then((result) => {
        setRows(result.data);
        setTotal(result.pagination.total);
        setTotalPages(result.pagination.total_pages);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(cause instanceof ApiClientError ? cause.message : "读取请求日志失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, filters, retry]);

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next: LogFilters = {};
    if (draft.request_id.trim()) next.request_id = draft.request_id.trim();
    if (draft.channel) next.channel = draft.channel;
    if (draft.model.trim()) next.model = draft.model.trim();
    if (draft.status) next.status = Number(draft.status);
    if (draft.error_kind) next.error_kind = draft.error_kind;
    if (draft.stream) next.stream = draft.stream === "true";
    if (draft.from) next.from = toUtcStart(draft.from);
    if (draft.to) next.to = toUtcEndExclusive(draft.to);
    setPage(1);
    setFilters(next);
  }

  function clearFilters() {
    setDraft(emptyFilters);
    setFilters({});
    setPage(1);
  }

  async function confirmLogClear() {
    if (!canClear) return;
    setClearing(true);
    setClearError("");
    setClearMessage("");
    try {
      const result = await clearExpiredLogs();
      setConfirmClear(false);
      setClearMessage(
        `清理完成：删除 ${result.request_logs_deleted} 条请求明细、${result.usage_daily_deleted} 条日用量记录。`,
      );
      setPage(1);
      setRetry((value) => value + 1);
    } catch (cause: unknown) {
      setClearError(cause instanceof ApiClientError ? cause.message : "清理过期日志失败");
    } finally {
      setClearing(false);
    }
  }

  return (
    <main className="page-content data-page logs-page">
      <div className="page-heading">
        <div>
          <h1>请求日志</h1>
          <p>共 {new Intl.NumberFormat("zh-CN").format(total)} 条 · 时间为 UTC</p>
        </div>
        {canClear && (
          <button
            className="secondary-action-button danger-action"
            type="button"
            onClick={() => setConfirmClear(true)}
            disabled={clearing}
          >
            清理过期日志
          </button>
        )}
      </div>

      {confirmClear && (
        <div className="notice notice-confirm" role="alertdialog" aria-label="确认清理过期日志">
          <span>将按服务端配置删除保留期外的请求明细和日用量记录。此操作不可撤销，继续？</span>
          <div className="log-filter-actions">
            <button type="button" disabled={clearing} onClick={() => void confirmLogClear()}>
              {clearing ? "清理中…" : "确认清理"}
            </button>
            <button
              type="button"
              className="secondary-action"
              disabled={clearing}
              onClick={() => setConfirmClear(false)}
            >取消</button>
          </div>
        </div>
      )}
      {clearMessage && <div className="notice notice-info" role="status">{clearMessage}</div>}
      {clearError && <div className="notice notice-error" role="alert">{clearError}</div>}

      <form className="log-filter-form" onSubmit={applyFilters}>
        <label>
          <span>请求 ID</span>
          <input
            value={draft.request_id}
            onChange={(event) => setDraft({ ...draft, request_id: event.target.value })}
            placeholder="精确匹配"
          />
        </label>
        <label>
          <span>渠道</span>
          <select
            value={draft.channel}
            onChange={(event) => setDraft({ ...draft, channel: event.target.value })}
          >
            <option value="">全部渠道</option>
            {channels.map((channel) => <option key={channel.slug} value={channel.slug}>{channel.name}</option>)}
          </select>
        </label>
        <label>
          <span>模型</span>
          <input
            value={draft.model}
            onChange={(event) => setDraft({ ...draft, model: event.target.value })}
            placeholder="包含匹配"
          />
        </label>
        <label>
          <span>HTTP 状态</span>
          <input
            type="number"
            min="100"
            max="599"
            value={draft.status}
            onChange={(event) => setDraft({ ...draft, status: event.target.value })}
            placeholder="全部"
          />
        </label>
        <label>
          <span>错误分类</span>
          <input
            value={draft.error_kind}
            onChange={(event) => setDraft({ ...draft, error_kind: event.target.value })}
            placeholder="精确匹配"
          />
        </label>
        <label>
          <span>流式</span>
          <select
            value={draft.stream}
            onChange={(event) => setDraft({ ...draft, stream: event.target.value })}
          >
            <option value="">全部</option>
            <option value="true">是</option>
            <option value="false">否</option>
          </select>
        </label>
        <label>
          <span>从日期</span>
          <input type="date" value={draft.from} onChange={(event) => setDraft({ ...draft, from: event.target.value })} />
        </label>
        <label>
          <span>到日期</span>
          <input type="date" value={draft.to} onChange={(event) => setDraft({ ...draft, to: event.target.value })} />
        </label>
        <div className="log-filter-actions">
          <button type="submit">筛选</button>
          <button type="button" className="secondary-action" onClick={clearFilters}>清除</button>
        </div>
      </form>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button>
        </div>
      )}

      {loading ? (
        <div className="table-state" aria-live="polite">正在读取请求日志…</div>
      ) : rows.length === 0 ? (
        <div className="table-state">没有符合条件的请求</div>
      ) : (
        <div className="table-wrap">
          <table className="data-table log-table">
            <thead>
              <tr>
                <th>时间 (UTC)</th><th>请求</th><th>渠道 / 模型</th><th>状态</th>
                <th>耗时</th><th>Token</th><th>用量</th><th>降级</th>
              </tr>
            </thead>
            <tbody>{rows.map((row) => <LogRow key={row.id} row={row} />)}</tbody>
          </table>
        </div>
      )}

      <div className="log-pagination">
        <span>第 {page} / {Math.max(totalPages, 1)} 页</span>
        <div>
          <button type="button" disabled={loading || page <= 1} onClick={() => setPage((value) => value - 1)}>上一页</button>
          <button type="button" disabled={loading || page >= totalPages} onClick={() => setPage((value) => value + 1)}>下一页</button>
        </div>
      </div>
    </main>
  );
}

function LogRow({ row }: { row: RequestLog }) {
  const tokens = row.prompt_tokens + row.completion_tokens;
  return (
    <tr>
      <td>{formatTimestamp(row.ts)}</td>
      <td>
        <span className="log-request-id">{row.request_id}</span>
        {row.error_kind && <span className="secondary-text">{row.error_kind}</span>}
      </td>
      <td>
        <span className="channel-name">{row.channel ?? "—"}</span>
        <span className="secondary-text">{row.model ?? row.upstream_model ?? "—"}</span>
      </td>
      <td><span className={`status-label ${statusTone(row.status)}`}>{row.status}</span></td>
      <td>{row.latency_ms.toLocaleString("zh-CN")} ms</td>
      <td>{row.usage_reported ? tokens.toLocaleString("zh-CN") : "未知"}</td>
      <td>{row.stream ? "流式" : "非流式"}</td>
      <td>{row.fallback_depth}</td>
    </tr>
  );
}
