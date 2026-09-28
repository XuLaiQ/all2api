import { useEffect, useState, type FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { createUser, deleteUser, fetchUsers, patchUser, type ManagedUser } from "./managementApi";

export function UsersPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManage = role === "admin";
  const [users, setUsers] = useState<ManagedUser[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState({ username: "", role: "viewer" as "admin" | "viewer" });
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState("");

  async function load() {
    setLoading(true);
    setError("");
    try {
      setUsers((await fetchUsers()).data);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "读取用户失败");
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { void load(); }, []);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!canManage || !draft.username.trim()) return;
    setSaving(true);
    setError("");
    try {
      await createUser({ username: draft.username.trim(), role: draft.role, enabled: true });
      setDraft({ username: "", role: "viewer" });
      setNotice("用户已创建");
      await load();
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "创建用户失败");
    } finally { setSaving(false); }
  }

  async function toggle(user: ManagedUser) {
    try { await patchUser(user.username, { enabled: !user.enabled }); await load(); }
    catch (cause: unknown) { setError(cause instanceof ApiClientError ? cause.message : "更新用户失败"); }
  }

  async function remove(user: ManagedUser) {
    if (!window.confirm(`确定删除用户“${user.username}”吗？`)) return;
    try { await deleteUser(user.username); setNotice("用户已删除"); await load(); }
    catch (cause: unknown) { setError(cause instanceof ApiClientError ? cause.message : "删除用户失败"); }
  }

  return <main className="page-content data-page management-page">
    <div className="page-heading"><div><span className="page-eyebrow">ACCESS CONTROL</span><h1>用户管理</h1><p>管理控制台用户目录；认证凭据仍由环境配置维护</p></div><button className="secondary-action-button" type="button" onClick={() => void load()}>刷新</button></div>
    {error && <div className="notice notice-error" role="alert">{error}</div>}
    {notice && <div className="notice notice-info" role="status">{notice}</div>}
    {canManage && <form className="log-filter-form management-form" onSubmit={(event) => void submit(event)}>
      <label><span>用户名</span><input value={draft.username} onChange={(event) => setDraft({ ...draft, username: event.target.value })} placeholder="例如 operator" /></label>
      <label><span>角色</span><select value={draft.role} onChange={(event) => setDraft({ ...draft, role: event.target.value as "admin" | "viewer" })}><option value="viewer">Viewer</option><option value="admin">Admin</option></select></label>
      <button type="submit" disabled={saving || !draft.username.trim()}>{saving ? "创建中…" : "创建用户"}</button>
    </form>}
    {loading ? <div className="table-state">正在读取用户…</div> : <div className="table-wrap"><table className="data-table"><thead><tr><th>用户名</th><th>角色</th><th>状态</th><th>更新时间</th>{canManage && <th>操作</th>}</tr></thead><tbody>
      {users.map((user) => <tr key={user.username}><td className="channel-name">{user.username}</td><td>{user.role}</td><td>{user.enabled ? "启用" : "停用"}</td><td>{new Date(user.updated_at * 1000).toLocaleString("zh-CN")}</td>{canManage && <td className="account-actions"><button type="button" className="secondary-action compact-action" onClick={() => void toggle(user)}>{user.enabled ? "停用" : "启用"}</button><button type="button" className="danger-action compact-action" onClick={() => void remove(user)}>删除</button></td>}</tr>)}
    </tbody></table></div>}
  </main>;
}
