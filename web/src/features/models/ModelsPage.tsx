import { useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import { fetchModels, setModelEnabled, type ModelFilters, type ModelRecord } from "./modelsApi";
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
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [pending, setPending] = useState<ModelRecord | null>(null);
  const [saving, setSaving] = useState(false);
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    fetchChannels(controller.signal).then(setChannels).catch(() => undefined);
    return () => controller.abort();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchModels(page, filters, controller.signal)
      .then((result) => {
        setRows(result.data);
        setKinds(result.facets.kinds);
        setTotal(result.pagination.total);
        setTotalPages(result.pagination.total_pages);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof ApiClientError ? cause.message : "读取模型目录失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, filters, retry]);

  function applyFilters() {
    const next: ModelFilters = {};
    if (draft.channel) next.channel = draft.channel;
    if (draft.kind) next.kind = draft.kind;
    if (draft.enabled) next.enabled = draft.enabled === "true";
    if (draft.search.trim()) next.search = draft.search.trim();
    setFilters(next);
    setPage(1);
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
          <span className="page-eyebrow">MODEL CATALOG</span>
          <h1>模型目录</h1>
          <p>{total} 个已观测模型 · 本地缓存，不代表上游实时探测</p>
        </div>
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button>
        </div>
      )}
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
          onChange={(channel) => setDraft({ ...draft, channel })}
          options={[
            { value: "", label: "全部渠道" },
            ...channels.map((channel) => ({ value: channel.slug, label: channel.name })),
          ]}
        /></label>
        <label><span>类型</span><Select
          value={draft.kind}
          onChange={(kind) => setDraft({ ...draft, kind })}
          options={[
            { value: "", label: "全部类型" },
            ...kinds.map((kind) => ({ value: kind, label: kind })),
          ]}
        /></label>
        <label><span>状态</span><Select
          value={draft.enabled}
          onChange={(enabled) => setDraft({ ...draft, enabled })}
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
        <div className="table-state" aria-live="polite">正在读取本地模型缓存…</div>
      ) : rows.length === 0 ? (
        <div className="table-state">本地还没有已观测模型。模型调用方访问 `GET /v1/models` 后，目录缓存才会出现记录。</div>
      ) : (
        <div className="table-wrap">
          <table className="data-table model-table">
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
