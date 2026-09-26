import { useEffect, useState } from "react";
import { ApiClientError } from "../../api/client";
import {
  fetchStorageHealth,
  fetchSystemInfo,
  fetchSystemMetrics,
  type StorageHealth,
  type SystemInfo,
  type SystemMetrics,
} from "./systemApi";

function bytes(value: number | undefined): string {
  if (value === undefined) return "—";
  if (value < 1024) return `${value} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let scaled = value;
  let index = -1;
  while (scaled >= 1024 && index < units.length - 1) {
    scaled /= 1024;
    index += 1;
  }
  return `${scaled.toFixed(scaled >= 10 ? 0 : 1)} ${units[index]}`;
}

function percent(value: number): string {
  return `${(value * 100).toFixed(2)}%`;
}

function stateLabel(status: string): { label: string; tone: string } {
  if (status === "ok") return { label: "正常", tone: "success" };
  if (status === "not_initialized") return { label: "未初始化", tone: "neutral" };
  return { label: "需检查", tone: "danger" };
}

export function SystemPage() {
  const [info, setInfo] = useState<SystemInfo | null>(null);
  const [storage, setStorage] = useState<StorageHealth | null>(null);
  const [metrics, setMetrics] = useState<SystemMetrics | null>(null);
  const [days, setDays] = useState(1);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    Promise.all([
      fetchSystemInfo(controller.signal),
      fetchStorageHealth(controller.signal),
      fetchSystemMetrics(days, controller.signal),
    ])
      .then(([nextInfo, nextStorage, nextMetrics]) => {
        setInfo(nextInfo);
        setStorage(nextStorage);
        setMetrics(nextMetrics);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof ApiClientError ? cause.message : "读取系统状态失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [days, retry]);

  const runtime = stateLabel(info?.runtime_state.status ?? "not_initialized");
  const storageState = storage?.status === "ok" ? { label: "正常", tone: "success" } : { label: "降级", tone: "danger" };

  return (
    <main className="page-content data-page system-page">
      <div className="page-heading">
        <div>
          <span className="page-eyebrow">SYSTEM HEALTH</span>
          <h1>系统健康</h1>
          <p>本地网关、存储和请求观测 · 不自动探测上游</p>
        </div>
        <div className="system-page-actions">
          <label><span className="visually-hidden">指标窗口</span><select value={days} onChange={(event) => setDays(Number(event.target.value))}>
            <option value={1}>最近 24 小时</option>
            <option value={7}>最近 7 天</option>
            <option value={30}>最近 30 天</option>
          </select></label>
          <button className="secondary-action-button" type="button" onClick={() => setRetry((value) => value + 1)} disabled={loading}>刷新</button>
        </div>
      </div>

      {error && <div className="notice notice-error" role="alert"><span>{error}</span><button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button></div>}
      {loading ? (
        <div className="table-state" aria-live="polite">正在读取系统状态…</div>
      ) : (
        <>
          <section className="metric-grid system-metric-grid" aria-label="系统指标">
            <Metric label="请求数" value={metrics?.requests.toLocaleString("zh-CN") ?? "0"} />
            <Metric label="错误率" value={percent(metrics?.error_rate ?? 0)} />
            <Metric label="P95 延迟" value={`${Math.round(metrics?.p95_latency_ms ?? 0)} ms`} />
            <Metric label="可用账号" value={`${metrics?.accounts.available ?? 0} / ${metrics?.accounts.total ?? 0}`} />
          </section>

          <div className="system-sections">
            <section className="data-section surface-panel" aria-labelledby="service-info-title">
              <div className="section-heading"><div><h2 id="service-info-title">运行时</h2><p>服务版本与数据库状态</p></div><span className={`state-label state-${storageState.tone}`}><span className="status-mark" aria-hidden="true" />{storageState.label}</span></div>
              <dl className="system-detail-grid">
                <Detail label="服务" value={info?.service ?? "—"} />
                <Detail label="版本" value={info?.version ?? "—"} />
                <Detail label="Python" value={info ? `${info.python_version} · ${info.python_implementation}` : "—"} />
                <Detail label="Schema" value={String(info?.schema_version ?? "—")} />
                <Detail label="数据库" value={info?.database.present ? bytes(info.database.bytes) : "未创建"} />
                <Detail label="运行态文件" value={runtime.label} />
              </dl>
            </section>

            <section className="data-section surface-panel" aria-labelledby="storage-title">
              <div className="section-heading"><div><h2 id="storage-title">存储</h2><p>数据库所在磁盘容量</p></div><span className={`state-label state-${storageState.tone}`}><span className="status-mark" aria-hidden="true" />{storageState.label}</span></div>
              <dl className="system-detail-grid">
                <Detail label="数据库" value={storage?.database.status ?? "—"} />
                <Detail label="磁盘总量" value={bytes(storage?.disk.total_bytes)} />
                <Detail label="已使用" value={bytes(storage?.disk.used_bytes)} />
                <Detail label="可用空间" value={bytes(storage?.disk.free_bytes)} />
                <Detail label="运行态文件" value={runtime.label} />
                <Detail label="观测账号" value={String(metrics?.accounts.runtime_observed ?? 0)} />
              </dl>
            </section>
          </div>

          <section className="data-section surface-panel" aria-labelledby="channel-config-title">
            <div className="section-heading"><div><h2 id="channel-config-title">渠道配置</h2><p>配置状态来自本地环境，不代表上游在线</p></div></div>
            <div className="table-wrap"><table className="data-table system-channel-table"><thead><tr><th>渠道</th><th>模型接口</th><th>账号接口</th></tr></thead><tbody>
              {(info?.channels ?? []).map((channel) => <tr key={channel.slug}><td className="channel-name">{channel.slug}</td><td>{channel.models_configured ? "已配置" : "未配置"}</td><td>{channel.accounts_configured ? "已配置" : "未配置"}</td></tr>)}
            </tbody></table></div>
          </section>
        </>
      )}
    </main>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return <div className="metric-tile"><span className="metric-label">{label}</span><strong className="metric-value">{value}</strong></div>;
}

function Detail({ label, value }: { label: string; value: string }) {
  return <div><dt>{label}</dt><dd>{value}</dd></div>;
}
