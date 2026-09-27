import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import { Select } from "../../app/controls/Select";
import { DateInput } from "../../app/controls/DateInput";
import {
  createKey,
  fetchKeys,
  revokeKey,
  rotateKey,
  updateKey,
  type ApiKeyRecord,
  type KeyFilters,
  type KeyInput,
} from "./keysApi";

type EditorState = {
  id?: number;
  name: string;
  channels: string[];
  modelsText: string;
  expires: string;
  rpm: string;
};

type FilterDraft = { search: string; enabled: string };
const emptyFilters: FilterDraft = { search: "", enabled: "" };

const emptyEditor: EditorState = {
  name: "",
  channels: [],
  modelsText: "*",
  expires: "",
  rpm: "0",
};

function timestamp(value: number | null): string {
  if (value === null) return "永不过期";
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "short",
    timeZone: "UTC",
  }).format(new Date(value * 1000));
}

function keyStatus(row: ApiKeyRecord): { label: string; className: string } {
  if (row.expires_at !== null && row.expires_at <= Math.floor(Date.now() / 1000)) {
    return { label: "过期", className: "status-warning" };
  }
  return row.enabled
    ? { label: "启用", className: "status-success" }
    : { label: "停用", className: "status-danger" };
}

function keyInput(editor: EditorState): KeyInput {
  const models = editor.modelsText.split(",").map((model) => model.trim()).filter(Boolean);
  const expiresAt = editor.expires
    ? Math.floor(new Date(`${editor.expires}T23:59:59Z`).getTime() / 1000)
    : null;
  return {
    name: editor.name.trim(),
    channels: editor.channels,
    models,
    expires_at: expiresAt,
    limit_rpm: Number(editor.rpm),
  };
}

export function KeysPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManage = role === "admin";
  const [rows, setRows] = useState<ApiKeyRecord[]>([]);
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [editor, setEditor] = useState<EditorState | null>(null);
  const [oneTimeKey, setOneTimeKey] = useState("");
  const [oneTimeTitle, setOneTimeTitle] = useState("");
  const [pendingAction, setPendingAction] = useState<{ id: number; kind: "rotate" | "revoke" } | null>(null);
  const [page, setPage] = useState(1);
  const [draftFilters, setDraftFilters] = useState<FilterDraft>(emptyFilters);
  const [filters, setFilters] = useState<KeyFilters>({});
  const [totalPages, setTotalPages] = useState(0);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
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
    fetchKeys(page, filters, controller.signal)
      .then((result) => {
        setRows(result.data);
        setTotal(result.pagination.total);
        setTotalPages(result.pagination.total_pages);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(cause instanceof ApiClientError ? cause.message : "读取密钥失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, filters, retry]);

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next: KeyFilters = {};
    if (draftFilters.search.trim()) next.search = draftFilters.search.trim();
    if (draftFilters.enabled) next.enabled = draftFilters.enabled === "true";
    setPage(1);
    setFilters(next);
  }

  function clearFilters() {
    setDraftFilters(emptyFilters);
    setPage(1);
    setFilters({});
  }

  function startEdit(row?: ApiKeyRecord) {
    if (!row) {
      setEditor({ ...emptyEditor });
      return;
    }
    setEditor({
      id: row.id,
      name: row.name,
      channels: row.channels,
      modelsText: row.models.join(", "),
      expires: row.expires_at
        ? new Date(row.expires_at * 1000).toISOString().slice(0, 10)
        : "",
      rpm: String(row.limit_rpm),
    });
  }

  async function saveEditor(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!editor) return;
    setSaving(true);
    setError("");
    try {
      const input = keyInput(editor);
      if (editor.id === undefined) {
        const created = await createKey(input);
        setOneTimeTitle("密钥已创建");
        setOneTimeKey(created.key);
      } else {
        await updateKey(editor.id, input);
      }
      setEditor(null);
      setRetry((value) => value + 1);
    } catch (cause) {
      setError(cause instanceof ApiClientError ? cause.message : "保存密钥失败");
    } finally {
      setSaving(false);
    }
  }

  async function toggleEnabled(row: ApiKeyRecord) {
    try {
      await updateKey(row.id, { enabled: !row.enabled });
      setRetry((value) => value + 1);
    } catch (cause) {
      setError(cause instanceof ApiClientError ? cause.message : "更新密钥状态失败");
    }
  }

  async function confirmPendingAction() {
    if (!pendingAction) return;
    const action = pendingAction;
    setPendingAction(null);
    try {
      if (action.kind === "rotate") {
        const rotated = await rotateKey(action.id);
        setOneTimeTitle("密钥已轮换，旧值立即失效");
        setOneTimeKey(rotated.key);
      } else {
        await revokeKey(action.id);
      }
      setRetry((value) => value + 1);
    } catch (cause) {
      setError(cause instanceof ApiClientError ? cause.message : "密钥操作失败");
    }
  }

  async function copyOneTimeKey() {
    try {
      await navigator.clipboard.writeText(oneTimeKey);
    } catch {
      setError("无法访问剪贴板，请手动复制当前密钥");
    }
  }

  return (
    <main className="page-content data-page keys-page">
      <div className="page-heading">
        <div>
          <h1>网关密钥</h1>
          <p>{total} 个密钥 · 明文只在创建或轮换时展示</p>
        </div>
        {canManage && <button className="key-primary-action" type="button" onClick={() => startEdit()}>创建密钥</button>}
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button>
        </div>
      )}
      {pendingAction && (
        <div className="notice notice-confirm" role="alertdialog" aria-label="确认密钥操作">
          <span>
            {pendingAction.kind === "rotate"
              ? "轮换会立即使当前明文密钥失效。"
              : "撤销会立即停止该密钥的调用。"}
            确认继续？
          </span>
          <div className="log-filter-actions">
            <button type="button" onClick={confirmPendingAction}>确认</button>
            <button type="button" className="secondary-action" onClick={() => setPendingAction(null)}>取消</button>
          </div>
        </div>
      )}
      {oneTimeKey && (
        <section className="one-time-key" aria-labelledby="one-time-key-title">
          <div>
            <h2 id="one-time-key-title">{oneTimeTitle}</h2>
            <p>关闭此区域后无法再次查看完整值。请在继续前保存它。</p>
            <code>{oneTimeKey}</code>
          </div>
          <div className="log-filter-actions">
            <button type="button" onClick={copyOneTimeKey}>复制密钥</button>
            <button type="button" className="secondary-action" onClick={() => setOneTimeKey("")}>隐藏</button>
          </div>
        </section>
      )}

      <form className="log-filter-form key-filter-form" onSubmit={applyFilters}>
        <label>
          <span>名称或前缀</span>
          <input
            value={draftFilters.search}
            onChange={(event) => setDraftFilters({ ...draftFilters, search: event.target.value })}
            placeholder="搜索密钥名称或前缀"
          />
        </label>
        <label>
          <span>状态</span>
          <Select
            value={draftFilters.enabled}
            onChange={(enabled) => setDraftFilters({ ...draftFilters, enabled })}
            options={[
              { value: "", label: "全部状态" },
              { value: "true", label: "启用" },
              { value: "false", label: "停用" },
            ]}
          />
        </label>
        <div className="log-filter-actions">
          <button type="submit">筛选</button>
          <button type="button" className="secondary-action" onClick={clearFilters}>清除</button>
        </div>
      </form>

      {loading ? (
        <div className="table-state" aria-live="polite">正在读取密钥…</div>
      ) : rows.length === 0 ? (
        <div className="table-state">没有已创建的网关密钥</div>
      ) : (
        <div className="table-wrap">
          <table className="data-table key-table">
            <thead>
              <tr><th>名称 / 前缀</th><th>状态</th><th>渠道</th><th>模型</th><th>RPM</th><th>到期</th><th>最后使用</th><th>操作</th></tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const state = keyStatus(row);
                return (
                  <tr key={row.id}>
                    <td><span className="channel-name">{row.name}</span><span className="secondary-text">{row.prefix}…</span></td>
                    <td><span className={`status-label ${state.className}`}>{state.label}</span></td>
                    <td>{row.channels.length ? row.channels.join(", ") : "全部"}</td>
                    <td><span className="key-model-scope">{row.models.join(", ")}</span></td>
                    <td>{row.limit_rpm || "不限"}</td>
                    <td>{timestamp(row.expires_at)}</td>
                    <td>{row.last_used_at ? timestamp(row.last_used_at) : "尚未使用"}</td>
                    <td>
                      <div className="key-row-actions">
                        {canManage && <>
                          <button type="button" onClick={() => startEdit(row)} disabled={row.name === "bootstrap"}>编辑</button>
                          <button type="button" onClick={() => void toggleEnabled(row)} disabled={row.name === "bootstrap"}>
                            {row.enabled ? "停用" : "启用"}
                          </button>
                          <button type="button" onClick={() => setPendingAction({ id: row.id, kind: "rotate" })} disabled={row.name === "bootstrap"}>轮换</button>
                          <button type="button" className="danger-action" onClick={() => setPendingAction({ id: row.id, kind: "revoke" })} disabled={row.name === "bootstrap"}>撤销</button>
                        </>}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
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

      {editor && (
        <div className="key-modal-backdrop" role="presentation" onMouseDown={(event) => {
          if (event.target === event.currentTarget && !saving) setEditor(null);
        }}>
          <section className="key-editor" role="dialog" aria-modal="true" aria-labelledby="key-editor-title">
            <div className="section-heading">
              <div><h2 id="key-editor-title">{editor.id === undefined ? "创建网关密钥" : "编辑网关密钥"}</h2><p>密钥只存储哈希值</p></div>
              <button type="button" className="icon-close" aria-label="关闭" onClick={() => setEditor(null)}>×</button>
            </div>
            <form className="key-editor-form" onSubmit={saveEditor}>
              <label><span>名称</span><input required maxLength={128} value={editor.name} onChange={(event) => setEditor({ ...editor, name: event.target.value })} /></label>
              <fieldset>
                <legend>允许渠道</legend>
                <div className="key-channel-options">
                  {channels.map((channel) => (
                    <label key={channel.slug}>
                      <input
                        type="checkbox"
                        checked={editor.channels.includes(channel.slug)}
                        onChange={(event) => setEditor({
                          ...editor,
                          channels: event.target.checked
                            ? [...editor.channels, channel.slug]
                            : editor.channels.filter((item) => item !== channel.slug),
                        })}
                      />
                      {channel.name}
                    </label>
                  ))}
                  <small>不选表示全部渠道</small>
                </div>
              </fieldset>
              <label><span>模型范围</span><input required value={editor.modelsText} onChange={(event) => setEditor({ ...editor, modelsText: event.target.value })} /><small>用逗号分隔；`*` 表示全部模型</small></label>
              <div className="key-form-grid">
                <label><span>每分钟请求上限</span><input type="number" min="0" max="1000000" required value={editor.rpm} onChange={(event) => setEditor({ ...editor, rpm: event.target.value })} /><small>0 表示不限</small></label>
                <label><span>到期日 (UTC)</span><DateInput value={editor.expires} onChange={(expires) => setEditor({ ...editor, expires })} /><small>留空表示永不过期</small></label>
              </div>
              <div className="log-filter-actions">
                <button type="submit" disabled={saving}>{saving ? "保存中…" : "保存"}</button>
                <button type="button" className="secondary-action" onClick={() => setEditor(null)} disabled={saving}>取消</button>
              </div>
            </form>
          </section>
        </div>
      )}
    </main>
  );
}
