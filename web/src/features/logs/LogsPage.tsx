import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { DataTable, TableState } from "../../app/data/DataTable";
import { Pagination } from "../../app/data/Pagination";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "../../app/data/pagination.constants";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import { clearExpiredLogs, fetchLogs, type LogFilters, type RequestLog } from "./logsApi";
import { Select } from "../../app/controls/Select";
import { DateInput } from "../../app/controls/DateInput";

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
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [rows, setRows] = useState<RequestLog[]>([]);
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [total, setTotal] = useState(0);
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
    fetchLogs(page, filters, controller.signal, pageSize)
      .then((result) => {
        setRows(result.data);
        setTotal(result.pagination.total);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(cause instanceof ApiClientError ? cause.message : "读取请求日志失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, pageSize, filters, retry]);

  function applyDraftFilters(nextDraft: DraftFilters) {
    const next: LogFilters = {};
    if (nextDraft.request_id.trim()) next.request_id = nextDraft.request_id.trim();
    if (nextDraft.channel) next.channel = nextDraft.channel;
    if (nextDraft.model.trim()) next.model = nextDraft.model.trim();
    if (nextDraft.status) next.status = Number(nextDraft.status);
    if (nextDraft.error_kind) next.error_kind = nextDraft.error_kind;
    if (nextDraft.stream) next.stream = nextDraft.stream === "true";
    if (nextDraft.from) next.from = toUtcStart(nextDraft.from);
    if (nextDraft.to) next.to = toUtcEndExclusive(nextDraft.to);
    setDraft(nextDraft);
    setPage(1);
    setFilters(next);
  }

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    applyDraftFilters(draft);
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
          <Button
            variant="danger"
            onClick={() => setConfirmClear(true)}
            disabled={clearing}
          >
            清理过期日志
          </Button>
        )}
      </div>

      {confirmClear && (
        <div className="notice notice-confirm" role="alertdialog" aria-label="确认清理过期日志">
          <span>将按服务端配置删除保留期外的请求明细和日用量记录。此操作不可撤销，继续？</span>
          <div className="log-filter-actions">
            <Button variant="primary" disabled={clearing} onClick={() => void confirmLogClear()}>
              {clearing ? "清理中…" : "确认清理"}
            </Button>
            <Button
              variant="secondary"
              disabled={clearing}
              onClick={() => setConfirmClear(false)}
            >取消</Button>
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
          <Select
            value={draft.channel}
            onChange={(channel) => applyDraftFilters({ ...draft, channel })}
            options={[
              { value: "", label: "全部渠道" },
              ...channels.map((channel) => ({ value: channel.slug, label: channel.name })),
            ]}
          />
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
          <Select
            value={draft.stream}
            onChange={(stream) => applyDraftFilters({ ...draft, stream })}
            options={[
              { value: "", label: "全部" },
              { value: "true", label: "是" },
              { value: "false", label: "否" },
            ]}
          />
        </label>
        <label>
          <span>从日期</span>
          <DateInput value={draft.from} onChange={(from) => setDraft({ ...draft, from })} />
        </label>
        <label>
          <span>到日期</span>
          <DateInput value={draft.to} onChange={(to) => setDraft({ ...draft, to })} />
        </label>
        <div className="log-filter-actions">
          <Button type="submit" variant="primary">筛选</Button>
          <Button variant="secondary" onClick={clearFilters}>清除</Button>
        </div>
      </form>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <Button variant="secondary" size="sm" onClick={() => setRetry((value) => value + 1)}>重试</Button>
        </div>
      )}

      {loading ? (
        <TableState live>正在读取请求日志…</TableState>
      ) : rows.length === 0 ? (
        <TableState>没有符合条件的请求</TableState>
      ) : (
        <DataTable className="log-table" ariaLabel="请求日志列表">
            <thead>
              <tr>
                <th>时间 (UTC)</th><th>请求</th><th>渠道 / 模型</th><th>状态</th>
                <th>耗时</th><th>Token</th><th>用量</th><th>降级</th>
              </tr>
            </thead>
            <tbody>{rows.map((row) => <LogRow key={row.id} row={row} />)}</tbody>
        </DataTable>
      )}

      <Pagination
        currentPage={page}
        pageSize={pageSize}
        total={total}
        pageSizes={PAGE_SIZE_OPTIONS}
        disabled={loading}
        onCurrentChange={setPage}
        onSizeChange={(nextPageSize) => { setPage(1); setPageSize(nextPageSize); }}
      />
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
      <td>{row.usage_kind === "unknown" ? "未知" : `${tokens.toLocaleString("zh-CN")}（${row.usage_kind === "estimated" ? "估算" : "已报告"}）`}</td>
      <td>{row.stream ? "流式" : "非流式"}</td>
      <td>{row.fallback_depth}</td>
    </tr>
  );
}
