import { useEffect, useState, type FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { DataTable } from "../../app/data/DataTable";
import { Pagination } from "../../app/data/Pagination";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "../../app/data/pagination.constants";
import { Select } from "../../app/controls/Select";
import { createUser, deleteUser, fetchUsers, patchUser, type ManagedUser } from "./managementApi";

export function UsersPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManage = role === "admin";
  const [users, setUsers] = useState<ManagedUser[]>([]);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState({ username: "", role: "viewer" as "admin" | "viewer" });
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState("");
  const [reload, setReload] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchUsers(page, pageSize, controller.signal)
      .then((result) => {
        setUsers(result.data);
        setTotal(result.pagination.total);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) setError(cause instanceof ApiClientError ? cause.message : "读取用户失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, pageSize, reload]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!canManage || !draft.username.trim()) return;
    setSaving(true);
    setError("");
    try {
      await createUser({ username: draft.username.trim(), role: draft.role, enabled: true });
      setDraft({ username: "", role: "viewer" });
      setNotice("用户已创建");
      setPage(1);
      setReload((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "创建用户失败");
    } finally { setSaving(false); }
  }

  async function toggle(user: ManagedUser) {
    try { await patchUser(user.username, { enabled: !user.enabled }); setReload((value) => value + 1); }
    catch (cause: unknown) { setError(cause instanceof ApiClientError ? cause.message : "更新用户失败"); }
  }

  async function remove(user: ManagedUser) {
    if (!window.confirm(`确定删除用户“${user.username}”吗？`)) return;
    try {
      await deleteUser(user.username);
      setNotice("用户已删除");
      if (users.length === 1 && page > 1) setPage((value) => value - 1);
      setReload((value) => value + 1);
    }
    catch (cause: unknown) { setError(cause instanceof ApiClientError ? cause.message : "删除用户失败"); }
  }

  return <main className="page-content data-page management-page">
    <div className="page-heading"><div><span className="page-eyebrow">ACCESS CONTROL</span><h1>用户管理</h1><p>管理控制台用户目录；认证凭据仍由环境配置维护</p></div><Button variant="secondary" onClick={() => setReload((value) => value + 1)}>刷新</Button></div>
    {error && <div className="notice notice-error" role="alert">{error}</div>}
    {notice && <div className="notice notice-info" role="status">{notice}</div>}
    {canManage && <form className="log-filter-form management-form" onSubmit={(event) => void submit(event)}>
      <label><span>用户名</span><input value={draft.username} onChange={(event) => setDraft({ ...draft, username: event.target.value })} placeholder="例如 operator" /></label>
      <label><span>角色</span><Select
        value={draft.role}
        onChange={(role) => setDraft({ ...draft, role: role as "admin" | "viewer" })}
        options={[
          { value: "viewer", label: "Viewer" },
          { value: "admin", label: "Admin" },
        ]}
      /></label>
      <Button type="submit" variant="primary" disabled={saving || !draft.username.trim()}>{saving ? "创建中…" : "创建用户"}</Button>
    </form>}
    {loading ? <div className="table-state">正在读取用户…</div> : <DataTable className={canManage ? "has-row-actions" : ""} ariaLabel="用户列表"><thead><tr><th>用户名</th><th>角色</th><th>状态</th><th>更新时间</th>{canManage && <th>操作</th>}</tr></thead><tbody>
      {users.map((user) => <tr key={user.username}><td className="channel-name">{user.username}</td><td>{user.role}</td><td>{user.enabled ? "启用" : "停用"}</td><td>{new Date(user.updated_at * 1000).toLocaleString("zh-CN")}</td>{canManage && <td className="account-actions"><Button variant="secondary" size="sm" className="compact-action" onClick={() => void toggle(user)}>{user.enabled ? "停用" : "启用"}</Button><Button variant="danger" size="sm" className="compact-action" onClick={() => void remove(user)}>删除</Button></td>}</tr>)}
    </tbody></DataTable>}
    <Pagination
      currentPage={page}
      pageSize={pageSize}
      total={total}
      pageSizes={PAGE_SIZE_OPTIONS}
      disabled={loading}
      onCurrentChange={setPage}
      onSizeChange={(nextPageSize) => { setPage(1); setPageSize(nextPageSize); }}
    />
  </main>;
}
