import { useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { DataTable, TableState } from "../../app/data/DataTable";
import { fetchChannelRuntime, resetChannelOverride, setChannelEnabled, testChannel, type ChannelRuntime, type ChannelTestResult } from "./channelsApi";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";

function runtimeLabel(state: string): { label: string; tone: string } {
  if (state === "breaker_open") return { label: "熔断", tone: "danger" };
  if (state === "cooldown") return { label: "冷却", tone: "warning" };
  return { label: "无冷却 / 熔断", tone: "success" };
}

function timestamp(value: number | null | undefined): string {
  if (!value) return "—";
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "short",
    timeZone: "UTC",
  }).format(new Date(value * 1000));
}

function remaining(seconds: number | null): string {
  if (!seconds || seconds <= 0) return "—";
  if (seconds < 60) return `${seconds} 秒`;
  return `${Math.ceil(seconds / 60)} 分钟`;
}

function accountAccessLabel(channel: ChannelOverview): { label: string; detail: string } {
  if (channel.provision_configured) {
    return { label: "原生流程可用", detail: "新增 / 导入账号" };
  }
  if (channel.accounts_configured) {
    return { label: "兼容接口可用", detail: "迁移期 bridge" };
  }
  return { label: "未配置", detail: "暂无账号入口" };
}

export function ChannelsPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManage = role === "admin";
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [selected, setSelected] = useState("");
  const [states, setStates] = useState<ChannelRuntime[]>([]);
  const [loading, setLoading] = useState(true);
  const [runtimeLoading, setRuntimeLoading] = useState(false);
  const [error, setError] = useState("");
  const [runtimeError, setRuntimeError] = useState("");
  const [testResult, setTestResult] = useState<ChannelTestResult | null>(null);
  const [testLoading, setTestLoading] = useState(false);
  const [testError, setTestError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchChannels(controller.signal)
      .then((items) => {
        setChannels(items);
        setSelected((current) => current && items.some((item) => item.slug === current)
          ? current
          : items[0]?.slug ?? "");
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof ApiClientError ? cause.message : "读取渠道失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [retry]);

  useEffect(() => {
    if (!selected) {
      setStates([]);
      return;
    }
    const controller = new AbortController();
    setRuntimeLoading(true);
    setRuntimeError("");
    fetchChannelRuntime(selected, controller.signal)
      .then(setStates)
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setRuntimeError(cause instanceof ApiClientError ? cause.message : "读取渠道运行态失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setRuntimeLoading(false);
      });
    return () => controller.abort();
  }, [selected, retry]);

  useEffect(() => {
    setTestResult(null);
    setTestError("");
  }, [selected]);

  async function runChannelTest() {
    if (!selected || !selectedChannel?.enabled) return;
    setTestLoading(true);
    setTestError("");
    try {
      setTestResult(await testChannel(selected));
    } catch (cause: unknown) {
      setTestResult(null);
      setTestError(cause instanceof ApiClientError ? cause.message : "渠道测试失败");
    } finally {
      setTestLoading(false);
    }
  }

  const selectedChannel = channels.find((channel) => channel.slug === selected);
  const channelRuntime = selectedChannel?.enabled ? states.find((state) => state.model === null) : undefined;
  const currentState = selectedChannel?.enabled
    ? runtimeLabel(channelRuntime?.state ?? "closed")
    : { label: "未配置", tone: "neutral" };

  async function toggleSelectedChannel() {
    if (!selectedChannel || !canManage) return;
    try {
      await setChannelEnabled(selectedChannel.slug, !selectedChannel.enabled);
      setRetry((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "更新渠道状态失败");
    }
  }

  async function resetSelectedChannel() {
    if (!selectedChannel || !canManage) return;
    if (!window.confirm(`确定重置“${selectedChannel.name}”的本地覆盖配置吗？`)) return;
    try {
      await resetChannelOverride(selectedChannel.slug);
      setRetry((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "重置渠道配置失败");
    }
  }

  return (
    <main className={`page-content data-page channels-page${selectedChannel ? " has-runtime" : ""}`}>
      <div className="page-heading">
        <div>
          <h1>渠道</h1>
          <p>配置完整度与网关冷却、熔断记录</p>
        </div>
        <button className="secondary-action-button" type="button" onClick={() => setRetry((value) => value + 1)} disabled={loading || runtimeLoading}>
          刷新状态
        </button>
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button>
        </div>
      )}

      {loading ? (
        <TableState live>正在读取渠道…</TableState>
      ) : channels.length === 0 ? (
        <TableState>没有已注册渠道</TableState>
      ) : (
        <DataTable className="channel-table" ariaLabel="渠道列表">
            <thead><tr><th>渠道</th><th>运行态</th><th>适配器</th><th>数据面状态</th><th>账号接入</th><th>入站协议</th><th>渠道能力声明</th></tr></thead>
            <tbody>
              {channels.map((channel) => {
                const isSelected = channel.slug === selected;
                const state = channel.enabled
                  ? runtimeLabel(channel.slug === selected ? channelRuntime?.state ?? "closed" : channel.state)
                  : { label: "未配置", tone: "neutral" };
                const accountAccess = accountAccessLabel(channel);
                return (
                  <tr key={channel.slug} className={isSelected ? "selected-row" : ""}>
                    <td><button type="button" className="channel-select-button" aria-pressed={isSelected} onClick={() => setSelected(channel.slug)}>
                      <span className="channel-name">{channel.name}</span><span className="secondary-text">{channel.slug}</span>
                    </button></td>
                    <td><span className={`state-label state-${state.tone}`}><span className="status-mark" aria-hidden="true" />{state.label}</span></td>
                    <td>{channel.adapter}</td>
                    <td><span className={`status-label ${channel.management_enabled === false ? "status-danger" : channel.data_plane_configured ? "status-success" : "status-warning"}`}>{channel.management_enabled === false ? "已停用" : channel.data_plane_configured ? "已配置" : "待配置"}</span><span className="secondary-text">{channel.management_enabled === false ? "本地管理开关" : "adapter / 账号状态"}</span></td>
                    <td><span className={`status-label ${channel.provision_configured ? "status-success" : channel.accounts_configured ? "status-warning" : "status-danger"}`}>{accountAccess.label}</span><span className="secondary-text">{accountAccess.detail}</span></td>
                    <td><span className="channel-tags">{channel.protocols.join(", ")}</span></td>
                    <td><span className="channel-tags">{channel.caps.join(", ")}</span><span className="secondary-text">渠道级声明</span></td>
                  </tr>
                );
              })}
            </tbody>
        </DataTable>
      )}

      {selectedChannel && (
        <section className="data-section surface-panel" aria-labelledby="channel-runtime-title">
          <div className="section-heading">
            <div><h2 id="channel-runtime-title">{selectedChannel.name} 运行态</h2><p>数据面状态控制请求是否进入该渠道；账号接入负责本地账号新增、导入和凭据生命周期。</p></div>
            <div className="channel-runtime-actions">
              <span className={`state-label state-${currentState.tone}`}><span className="status-mark" aria-hidden="true" />{currentState.label}</span>
              <button
                className="secondary-action-button"
                type="button"
                onClick={runChannelTest}
                disabled={!selectedChannel.enabled || testLoading || runtimeLoading}
              >
                {testLoading ? "测试中…" : "测试连接"}
              </button>
              {canManage && <button className="secondary-action-button" type="button" onClick={() => void toggleSelectedChannel()}>
                {selectedChannel.enabled ? "停用渠道" : "启用渠道"}
              </button>}
              {canManage && <button className="danger-action compact-action" type="button" onClick={() => void resetSelectedChannel()}>重置覆盖</button>}
            </div>
          </div>
          {runtimeError && <div className="notice notice-error" role="alert">{runtimeError}</div>}
          {testError && <div className="notice notice-error" role="alert">{testError}</div>}
          {testResult && (
            <div className="inline-status" role="status">
              <span className="status-mark status-mark-success" aria-hidden="true" />
              已连接 · {testResult.model_count} 个模型 · {testResult.latency_ms} ms
            </div>
          )}
          {runtimeLoading ? (
            <TableState live>正在读取运行态…</TableState>
          ) : !selectedChannel.enabled ? (
            <TableState>渠道未配置，尚无网关运行态</TableState>
          ) : states.length === 0 ? (
            <TableState>暂无失败记录</TableState>
          ) : (
            <DataTable className="runtime-table" ariaLabel="渠道运行态列表">
                <thead><tr><th>范围</th><th>状态</th><th>连续失败</th><th>HTTP</th><th>错误分类</th><th>冷却剩余</th><th>熔断截止</th><th>更新时间</th></tr></thead>
                <tbody>
                  {states.map((state) => {
                    const label = runtimeLabel(state.state);
                    return <tr key={state.model ?? "channel"}>
                      <td><span className="channel-name">{state.model ?? "整个渠道"}</span></td>
                      <td><span className={`state-label state-${label.tone}`}><span className="status-mark" aria-hidden="true" />{label.label}</span></td>
                      <td>{state.consecutive_failures}</td>
                      <td>{state.last_status ?? "—"}</td>
                      <td>{state.last_error_kind ?? "—"}</td>
                      <td>{remaining(state.retry_after)}</td>
                      <td>{timestamp(state.breaker_until)}</td>
                      <td>{timestamp(state.updated_at)}</td>
                    </tr>;
                  })}
                </tbody>
            </DataTable>
          )}
        </section>
      )}
    </main>
  );
}
