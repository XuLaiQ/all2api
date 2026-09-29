import { useCallback, useEffect, useRef, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { RefreshCw } from "lucide-react";
import { ApiClientError } from "../../api/client";
import { DataTable, TableState } from "../../app/data/DataTable";
import { Pagination } from "../../app/data/Pagination";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "../../app/data/pagination.constants";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import { fetchModels, refreshModels, setModelEnabled, type ModelFilters, type ModelRecord } from "./modelsApi";
import { Select } from "../../app/controls/Select";

type DraftFilters = { channel: string; kind: string; enabled: string; search: string };
const emptyFilters: DraftFilters = { channel: "", kind: "", enabled: "", search: "" };

function formatCount(value: number | null): string {
  return value === null ? "未提供" : new Intl.NumberFormat("zh-CN").format(value);
}

export function ModelsPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManage = role === "admin";
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [rows, setRows] = useState<ModelRecord[]>([]);
  const [kinds, setKinds] = useState<string[]>([]);
  const [draft, setDraft] = useState<DraftFilters>(emptyFilters);
  const [filters, setFilters] = useState<ModelFilters>({});
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [pending, setPending] = useState<ModelRecord | null>(null);
  const [saving, setSaving] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [catalogMessage, setCatalogMessage] = useState("");
  const [retry, setRetry] = useState(0);
  const initialRefreshDone = useRef(false);

  useEffect(() => {
    const controller = new AbortController();
    fetchChannels(controller.signal).then(setChannels).catch(() => undefined);
    return () => controller.abort();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchModels(page, filters, controller.signal, pageSize)
      .then((result) => {
        setRows(result.data);
        setKinds(result.facets.kinds);
        setTotal(result.pagination.total);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof ApiClientError ? cause.message : "读取模型广场失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, pageSize, filters, retry]);

  const refreshCatalog = useCallback(async () => {
    setRefreshing(true);
    setError("");
    setCatalogMessage("");
    try {
      const result = await refreshModels();
      const failed = result.channels.filter((item) => item.status === "failed");
      setCatalogMessage(
        failed.length > 0
          ? `已同步 ${result.total} 个真实模型；${failed.map((item) => item.channel).join("、")} 渠道暂不可用或超时`
          : `已从真实渠道同步 ${result.total} 个模型`,
      );
      setRetry((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "刷新真实模型广场失败");
    } finally {
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    if (!canManage || initialRefreshDone.current) return;
    initialRefreshDone.current = true;
    void refreshCatalog();
  }, [canManage, refreshCatalog]);

  function applyDraftFilters(nextDraft: DraftFilters) {
    const next: ModelFilters = {};
    if (nextDraft.channel) next.channel = nextDraft.channel;
    if (nextDraft.kind) next.kind = nextDraft.kind;
    if (nextDraft.enabled) next.enabled = nextDraft.enabled === "true";
    if (nextDraft.search.trim()) next.search = nextDraft.search.trim();
    setDraft(nextDraft);
    setFilters(next);
    setPage(1);
  }

  function applyFilters() {
    applyDraftFilters(draft);
  }

  async function confirmToggle() {
    if (!pending) return;
    const selected = pending;
    setSaving(true);
    setError("");
    try {
      await setModelEnabled(selected.id, !selected.enabled);
      setPending(null);
      setRetry((value) => value + 1);
    } catch (cause) {
      setError(cause instanceof ApiClientError ? cause.message : "更新模型状态失败");
    } finally {
      setSaving(false);
    }
  }

  return (
    <main className="page-content data-page models-page">
      <div className="page-heading">
        <div>
          <span className="page-eyebrow">MODEL PLAZA</span>
          <h1>模型广场</h1>
          <p>{total} 个模型 · 目录来自真实渠道接口，结果缓存在本地</p>
        </div>
        {canManage && <button className="secondary-action-button" type="button" onClick={() => void refreshCatalog()} disabled={refreshing}>
          <RefreshCw size={15} aria-hidden="true" className={refreshing ? "spin" : undefined} />
          {refreshing ? "刷新中…" : "刷新真实目录"}
        </button>}
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button>
        </div>
      )}
      {catalogMessage && <div className="notice notice-info" role="status">{catalogMessage}</div>}
      {pending && (
        <div className="notice notice-confirm" role="alertdialog" aria-label="确认模型状态变更">
          <span>
            {pending.enabled ? "停用后，公开模型列表和后续请求将拒绝此模型。" : "启用后，此模型可再次通过网关调用。"}
            确认{pending.enabled ? "停用" : "启用"} {pending.id}？
          </span>
          <div className="log-filter-actions">
            <button type="button" disabled={saving} onClick={() => void confirmToggle()}>{saving ? "处理中…" : "确认"}</button>
            <button type="button" className="secondary-action" disabled={saving} onClick={() => setPending(null)}>取消</button>
          </div>
        </div>
      )}

      <div className="log-filter-form model-filter-form">
        <label><span>渠道</span><Select
          value={draft.channel}
          onChange={(channel) => applyDraftFilters({ ...draft, channel })}
          options={[
            { value: "", label: "全部渠道" },
            ...channels.map((channel) => ({ value: channel.slug, label: channel.name })),
          ]}
        /></label>
        <label><span>类型</span><Select
          value={draft.kind}
          onChange={(kind) => applyDraftFilters({ ...draft, kind })}
          options={[
            { value: "", label: "全部类型" },
            ...kinds.map((kind) => ({ value: kind, label: kind })),
          ]}
        /></label>
        <label><span>状态</span><Select
          value={draft.enabled}
          onChange={(enabled) => applyDraftFilters({ ...draft, enabled })}
          options={[
            { value: "", label: "全部状态" },
            { value: "true", label: "启用" },
            { value: "false", label: "停用" },
          ]}
        /></label>
        <label><span>模型</span><input value={draft.search} onChange={(event) => setDraft({ ...draft, search: event.target.value })} placeholder="模型 ID 或名称" onKeyDown={(event) => { if (event.key === "Enter") applyFilters(); }} /></label>
        <div className="log-filter-actions">
          <button type="button" onClick={applyFilters}>筛选</button>
          <button type="button" className="secondary-action" onClick={() => { setDraft(emptyFilters); setFilters({}); setPage(1); }}>清除</button>
        </div>
      </div>

      {loading ? (
        <TableState live>正在读取本地模型缓存…</TableState>
      ) : rows.length === 0 ? (
        <TableState>暂未读取到真实模型。请先刷新模型广场，并确认渠道账号或平台凭据可用。</TableState>
      ) : (
        <DataTable className={`model-table ${canManage ? "has-row-actions" : ""}`.trim()} ariaLabel="模型广场列表">
            <thead><tr><th>模型</th><th>渠道</th><th>类型</th><th>能力</th><th>上下文</th><th>最大输出</th><th>状态</th>{canManage && <th>操作</th>}</tr></thead>
            <tbody>{rows.map((model) => (
              <tr key={model.id}>
                <td><span className="channel-name">{model.display_name}</span><span className="secondary-text">{model.id}</span></td>
                <td>{model.channel}</td>
                <td>{model.kind || "未提供"}</td>
                <td><span className="model-cap-list">{model.caps.length ? model.caps.join(", ") : "未提供"}</span></td>
                <td>{formatCount(model.context_window)}</td>
                <td>{formatCount(model.max_output)}</td>
                <td><span className={`status-label ${model.enabled ? "status-success" : "status-danger"}`}>{model.enabled ? "启用" : "停用"}</span></td>
                {canManage && <td><button type="button" className={model.enabled ? "model-disable-button" : "model-enable-button"} onClick={() => setPending(model)}>{model.enabled ? "停用" : "启用"}</button></td>}
              </tr>
            ))}</tbody>
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
