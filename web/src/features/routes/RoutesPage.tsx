import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { DataTable, TableState } from "../../app/data/DataTable";
import { Select } from "../../app/controls/Select";
import {
  deleteRoute,
  fetchRouteChannels,
  fetchRoutes,
  saveRoute,
  type ModelRoute,
  type RegisteredChannel,
  type RouteInput,
} from "./routesApi";

type TargetDraft = { channel: string; model: string };
type EditorState = { alias: string; originalAlias?: string; enabled: boolean; targets: TargetDraft[] };

function newTarget(channels: RegisteredChannel[], used: string[] = []): TargetDraft {
  const first = channels.find((channel) => !used.includes(channel.slug));
  return { channel: first?.slug ?? "", model: "" };
}

function targetEditor(route?: ModelRoute): EditorState {
  return route
      ? {
        alias: route.alias,
        originalAlias: route.alias,
        enabled: route.enabled,
        targets: route.targets.map(({ channel, model }) => ({ channel, model })),
      }
    : { alias: "", enabled: true, targets: [{ channel: "", model: "" }] };
}

export function RoutesPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManage = role === "admin";
  const [routes, setRoutes] = useState<ModelRoute[]>([]);
  const [channels, setChannels] = useState<RegisteredChannel[]>([]);
  const [channelsError, setChannelsError] = useState(false);
  const [editor, setEditor] = useState<EditorState | null>(null);
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    fetchRouteChannels(controller.signal)
      .then(setChannels)
      .catch(() => {
        if (!controller.signal.aborted) setChannelsError(true);
      });
    return () => controller.abort();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchRoutes(controller.signal)
      .then(setRoutes)
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(cause instanceof ApiClientError ? cause.message : "读取路由失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [retry]);

  function updateTarget(index: number, change: Partial<TargetDraft>) {
    if (!editor) return;
    setEditor({
      ...editor,
      targets: editor.targets.map((target, targetIndex) =>
        targetIndex === index ? { ...target, ...change } : target,
      ),
    });
  }

  function moveTarget(index: number, direction: -1 | 1) {
    if (!editor) return;
    const next = [...editor.targets];
    const other = index + direction;
    if (other < 0 || other >= next.length) return;
    [next[index], next[other]] = [next[other], next[index]];
    setEditor({ ...editor, targets: next });
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!editor) return;
    if (editor.targets.some((target) => !target.channel)) {
      setError("请为每个目标选择渠道");
      return;
    }
    setSaving(true);
    setError("");
    const input: RouteInput = {
      strategy: "priority",
      enabled: editor.enabled,
      targets: editor.targets.map((target) => ({
        channel: target.channel,
        model: target.model.trim(),
      })),
    };
    try {
      await saveRoute(editor.alias.trim(), input);
      setEditor(null);
      setRetry((value) => value + 1);
    } catch (cause) {
      setError(cause instanceof ApiClientError ? cause.message : "保存路由失败");
    } finally {
      setSaving(false);
    }
  }

  async function confirmDelete() {
    if (!pendingDelete) return;
    const alias = pendingDelete;
    setPendingDelete(null);
    try {
      await deleteRoute(alias);
      setRetry((value) => value + 1);
    } catch (cause) {
      setError(cause instanceof ApiClientError ? cause.message : "删除路由失败");
    }
  }

  const usedChannels = editor?.targets.map((target) => target.channel) ?? [];

  return (
    <main className="page-content data-page routes-page">
      <div className="page-heading">
        <div>
          <h1>路由规则</h1>
          <p>按目标顺序尝试；仅在可重试失败时进入下一渠道</p>
        </div>
        {canManage && <Button variant="primary" onClick={() => setEditor(targetEditor())}>创建路由</Button>}
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <Button variant="secondary" size="sm" onClick={() => setRetry((value) => value + 1)}>重试</Button>
        </div>
      )}
      {channelsError && <div className="notice notice-error" role="alert">无法读取渠道目录，当前不能编辑路由。</div>}
      {pendingDelete && (
        <div className="notice notice-confirm" role="alertdialog" aria-label="确认删除路由">
          <span>删除路由「{pendingDelete}」后，使用该别名的请求将失败。确认删除？</span>
          <div className="log-filter-actions">
            <Button variant="danger" onClick={() => void confirmDelete()}>确认删除</Button>
            <Button variant="secondary" onClick={() => setPendingDelete(null)}>取消</Button>
          </div>
        </div>
      )}

      {loading ? (
        <TableState live>正在读取路由…</TableState>
      ) : routes.length === 0 ? (
        <TableState>还没有路由规则</TableState>
      ) : (
        <DataTable className={`route-table ${canManage ? "has-row-actions" : ""}`.trim()} ariaLabel="路由规则列表">
            <thead><tr><th>别名</th><th>状态</th><th>目标顺序</th><th>创建时间</th>{canManage && <th>操作</th>}</tr></thead>
            <tbody>
              {routes.map((route) => (
                <tr key={route.alias}>
                  <td><span className="channel-name">{route.alias}</span></td>
                  <td><span className={`status-label ${route.enabled ? "status-success" : "status-danger"}`}>{route.enabled ? "启用" : "停用"}</span></td>
                  <td>
                    <ol className="route-target-list">
                      {route.targets.map((target) => (
                        <li key={`${target.position}:${target.channel}`}>
                          <span className="route-channel">{target.channel}</span>
                          <span>{target.model}</span>
                        </li>
                      ))}
                    </ol>
                  </td>
                  <td>{new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeZone: "UTC" }).format(new Date(route.created_at * 1000))}</td>
                  {canManage && <td><div className="route-row-actions">
                    <Button variant="secondary" size="sm" onClick={() => setEditor(targetEditor(route))}>编辑</Button>
                    <Button variant="danger" size="sm" onClick={() => setPendingDelete(route.alias)}>删除</Button>
                  </div></td>}
                </tr>
              ))}
            </tbody>
        </DataTable>
      )}

      {editor && (
        <div className="key-modal-backdrop" role="presentation" onMouseDown={(event) => {
          if (event.target === event.currentTarget && !saving) setEditor(null);
        }}>
          <section className="key-editor route-editor" role="dialog" aria-modal="true" aria-labelledby="route-editor-title">
            <div className="section-heading">
              <div><h2 id="route-editor-title">{editor.originalAlias ? "编辑路由" : "创建路由"}</h2><p>目标顺序决定回退顺序</p></div>
              <Button variant="unstyled" className="icon-close" aria-label="关闭" onClick={() => setEditor(null)}>×</Button>
            </div>
            <form className="key-editor-form" onSubmit={submit}>
              <label>
                <span>别名</span>
                <input
                  required
                  pattern="[A-Za-z0-9][A-Za-z0-9._-]{0,63}"
                  maxLength={64}
                  value={editor.alias}
                  disabled={Boolean(editor.originalAlias)}
                  onChange={(event) => setEditor({ ...editor, alias: event.target.value })}
                />
              </label>
              <label className="route-enabled-toggle">
                <input type="checkbox" checked={editor.enabled} onChange={(event) => setEditor({ ...editor, enabled: event.target.checked })} />
                启用路由
              </label>
              <div className="route-target-editor-list">
                {editor.targets.map((target, index) => (
                  <div className="route-target-editor" key={index}>
                    <div className="route-target-title"><strong>目标 {index + 1}</strong><div>
                      <Button variant="unstyled" type="button" aria-label="上移目标" disabled={index === 0} onClick={() => moveTarget(index, -1)}>↑</Button>
                      <Button variant="unstyled" type="button" aria-label="下移目标" disabled={index === editor.targets.length - 1} onClick={() => moveTarget(index, 1)}>↓</Button>
                      <Button variant="unstyled" type="button" aria-label="移除目标" disabled={editor.targets.length <= 1} onClick={() => setEditor({ ...editor, targets: editor.targets.filter((_, item) => item !== index) })}>×</Button>
                    </div></div>
                    <div className="key-form-grid">
                      <label><span>渠道</span><Select
                        required
                        value={target.channel}
                        onChange={(channel) => updateTarget(index, { channel })}
                        placeholder="选择渠道"
                        options={channels
                          .filter((channel) => !usedChannels.includes(channel.slug) || channel.slug === target.channel)
                          .map((channel) => ({
                            value: channel.slug,
                            label: `${channel.name} (${channel.slug})${channel.enabled ? "" : " · 未配置"}`,
                          }))}
                      /></label>
                      <label><span>模型 ID</span><input required maxLength={256} value={target.model} placeholder="例如 glm-5.2" onChange={(event) => updateTarget(index, { model: event.target.value })} /></label>
                    </div>
                  </div>
                ))}
                <Button
                  variant="unstyled"
                  className="add-route-target"
                  disabled={editor.targets.length >= Math.min(8, channels.length) || usedChannels.length >= channels.length}
                  onClick={() => setEditor({ ...editor, targets: [...editor.targets, newTarget(channels, usedChannels)] })}
                >添加目标</Button>
              </div>
              <div className="log-filter-actions">
                <Button type="submit" variant="primary" disabled={saving || channelsError || channels.length === 0}>{saving ? "保存中…" : "保存路由"}</Button>
                <Button variant="secondary" disabled={saving} onClick={() => setEditor(null)}>取消</Button>
              </div>
            </form>
          </section>
        </div>
      )}
    </main>
  );
}
