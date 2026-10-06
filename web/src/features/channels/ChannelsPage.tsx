import { useEffect, useState } from "react";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { DataTable, TableState } from "../../app/data/DataTable";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";

function runtimeLabel(state: string): { label: string; tone: string } {
  if (state === "breaker_open") return { label: "熔断", tone: "danger" };
  if (state === "cooldown") return { label: "冷却", tone: "warning" };
  return { label: "无冷却 / 熔断", tone: "success" };
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
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchChannels(controller.signal)
      .then(setChannels)
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

  return (
    <main className="page-content data-page channels-page">
      <div className="page-heading">
        <div>
          <h1>渠道</h1>
          <p>渠道配置、账号接入与能力声明</p>
        </div>
        <Button variant="secondary" onClick={() => setRetry((value) => value + 1)} disabled={loading}>
          刷新渠道
        </Button>
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <Button variant="secondary" size="sm" onClick={() => setRetry((value) => value + 1)}>重试</Button>
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
              const state = channel.enabled
                ? runtimeLabel(channel.state)
                : { label: "未配置", tone: "neutral" };
              const accountAccess = accountAccessLabel(channel);
              return (
                <tr key={channel.slug}>
                  <td><span className="channel-name">{channel.name}</span><span className="secondary-text">{channel.slug}</span></td>
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
    </main>
  );
}
