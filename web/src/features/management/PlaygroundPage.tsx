import { useEffect, useState, type FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { DataTable } from "../../app/data/DataTable";
import { Select } from "../../app/controls/Select";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import { fetchPlaygroundRuns, runPlayground, type PlaygroundRun } from "./managementApi";

export function PlaygroundPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [runs, setRuns] = useState<PlaygroundRun[]>([]);
  const [channel, setChannel] = useState("");
  const [model, setModel] = useState("");
  const [message, setMessage] = useState("");
  const [response, setResponse] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function load() {
    try {
      const [channelResult, runResult] = await Promise.all([fetchChannels(), fetchPlaygroundRuns()]);
      setChannels(channelResult);
      setRuns(runResult.data);
      setChannel((current) => current || channelResult[0]?.slug || "");
    } catch (cause: unknown) { setError(cause instanceof ApiClientError ? cause.message : "读取调试台数据失败"); }
  }
  useEffect(() => { void load(); }, []);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (role !== "admin" || !channel || !model.trim() || !message.trim()) return;
    setBusy(true); setError(""); setResponse(null);
    try {
      const result = await runPlayground({ channel, model: model.trim(), messages: [{ role: "user", content: message }] });
      setResponse(result.response); await load();
    } catch (cause: unknown) { setError(cause instanceof ApiClientError ? cause.message : "调试请求失败"); }
    finally { setBusy(false); }
  }

  return <main className="page-content data-page management-page">
    <div className="page-heading"><div><span className="page-eyebrow">PLAYGROUND</span><h1>调试台</h1><p>管理员可对已配置渠道发起一次非流式文本请求</p></div><button className="secondary-action-button" type="button" onClick={() => void load()}>刷新</button></div>
    {error && <div className="notice notice-error" role="alert">{error}</div>}
    <section className="data-section surface-panel"><form className="management-playground-form" onSubmit={(event) => void submit(event)}>
      <label><span>渠道</span><Select
        value={channel}
        onChange={setChannel}
        disabled={role !== "admin"}
        placeholder="选择渠道"
        options={channels.map((item) => ({ value: item.slug, label: `${item.name} (${item.slug})` }))}
      /></label>
      <label><span>模型</span><input value={model} onChange={(event) => setModel(event.target.value)} placeholder="例如 model-a" disabled={role !== "admin"} /></label>
      <label className="management-message-field"><span>消息</span><textarea value={message} onChange={(event) => setMessage(event.target.value)} rows={5} placeholder="输入一条测试消息" disabled={role !== "admin"} /></label>
      <button type="submit" disabled={role !== "admin" || busy || !channel || !model.trim() || !message.trim()}>{busy ? "请求中…" : "发送请求"}</button>
      {role !== "admin" && <span className="secondary-text">当前角色仅可查看历史运行记录</span>}
    </form>{response !== null && <pre className="management-response">{JSON.stringify(response, null, 2)}</pre>}</section>
    <section className="data-section surface-panel"><div className="section-heading"><div><h2>运行记录</h2><p>只保存请求元数据和脱敏状态，不保存 token</p></div></div><DataTable ariaLabel="运行记录列表"><thead><tr><th>时间</th><th>渠道 / 模型</th><th>状态</th><th>HTTP</th><th>消息数</th><th>操作者</th></tr></thead><tbody>{runs.map((run) => <tr key={run.id}><td>{new Date(run.created_at * 1000).toLocaleString("zh-CN")}</td><td><span className="channel-name">{run.channel}</span><span className="secondary-text">{run.model}</span></td><td>{run.status}</td><td>{run.response_status ?? "—"}</td><td>{run.message_count}</td><td>{run.actor}</td></tr>)}</tbody></DataTable></section>
  </main>;
}
