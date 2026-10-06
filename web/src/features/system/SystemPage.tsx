import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { DataTable } from "../../app/data/DataTable";
import { Select } from "../../app/controls/Select";
import {
  fetchAdminSettings,
  fetchStorageHealth,
  fetchSystemInfo,
  fetchSystemMetrics,
  updateAdminSettings,
  type AdminSettings,
  type StorageHealth,
  type SystemInfo,
  type SystemMetrics,
} from "./systemApi";
import "./SystemPage.css";

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
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const [info, setInfo] = useState<SystemInfo | null>(null);
  const [storage, setStorage] = useState<StorageHealth | null>(null);
  const [metrics, setMetrics] = useState<SystemMetrics | null>(null);
  const [settings, setSettings] = useState<AdminSettings | null>(null);
  const [settingsDraft, setSettingsDraft] = useState<AdminSettings["values"]>({
    log_retention_days: 30,
    usage_retention_days: 365,
  });
  const [settingsSaving, setSettingsSaving] = useState(false);
  const [settingsMessage, setSettingsMessage] = useState("");
  const [settingsError, setSettingsError] = useState("");
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
      fetchAdminSettings(controller.signal),
    ])
      .then(([nextInfo, nextStorage, nextMetrics, nextSettings]) => {
        setInfo(nextInfo);
        setStorage(nextStorage);
        setMetrics(nextMetrics);
        setSettings(nextSettings);
        setSettingsDraft(nextSettings.values);
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

  async function saveSettings(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (role !== "admin") return;
    setSettingsSaving(true);
    setSettingsMessage("");
    setSettingsError("");
    try {
      const nextSettings = await updateAdminSettings(settingsDraft);
      setSettings(nextSettings);
      setSettingsDraft(nextSettings.values);
      setSettingsMessage("设置已保存");
    } catch (cause: unknown) {
      setSettingsError(cause instanceof ApiClientError ? cause.message : "保存设置失败");
    } finally {
      setSettingsSaving(false);
    }
  }

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
          <label><span className="visually-hidden">指标窗口</span><Select value={String(days)} onChange={(value) => setDays(Number(value))} options={[
            { value: "1", label: "最近 24 小时" },
            { value: "7", label: "最近 7 天" },
            { value: "30", label: "最近 30 天" },
          ]} /></label>
          <Button variant="secondary" onClick={() => setRetry((value) => value + 1)} disabled={loading}>刷新</Button>
        </div>
      </div>

      {error && <div className="notice notice-error" role="alert"><span>{error}</span><Button variant="secondary" size="sm" onClick={() => setRetry((value) => value + 1)}>重试</Button></div>}
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
            <div className="section-heading"><div><h2 id="channel-config-title">渠道能力</h2><p>状态来自本地适配器和账号接入能力，不代表上游在线</p></div></div>
            <DataTable className="system-channel-table" ariaLabel="渠道能力列表"><thead><tr><th>渠道</th><th>模型能力</th><th>账号接入</th></tr></thead><tbody>
              {(info?.channels ?? []).map((channel) => <tr key={channel.slug}><td className="channel-name">{channel.slug}</td><td>{channel.models_configured ? "已配置" : "未配置"}</td><td>{channel.provision_configured || channel.accounts_configured ? "已配置" : "未配置"}</td></tr>)}
            </tbody></DataTable>
          </section>

          <section className="data-section surface-panel" aria-labelledby="system-settings-title">
            <div className="section-heading"><div><h2 id="system-settings-title">保留策略</h2><p>控制本地请求日志和用量聚合的保留周期</p></div></div>
            {settingsError && <div className="notice notice-error" role="alert">{settingsError}</div>}
            {settingsMessage && <div className="notice notice-info" role="status">{settingsMessage}</div>}
            <form className="system-settings-form" onSubmit={(event) => void saveSettings(event)}>
              <label>
                <span>请求日志保留天数</span>
                <input
                  type="number"
                  min={1}
                  max={36500}
                  value={settingsDraft.log_retention_days}
                  onChange={(event) => setSettingsDraft((current) => ({ ...current, log_retention_days: Number(event.target.value) }))}
                  disabled={role !== "admin" || settingsSaving}
                />
                <small>当前来源：{settings?.sources.log_retention_days === "database" ? "数据库" : "环境配置"}</small>
              </label>
              <label>
                <span>用量聚合保留天数</span>
                <input
                  type="number"
                  min={1}
                  max={36500}
                  value={settingsDraft.usage_retention_days}
                  onChange={(event) => setSettingsDraft((current) => ({ ...current, usage_retention_days: Number(event.target.value) }))}
                  disabled={role !== "admin" || settingsSaving}
                />
                <small>必须不短于日志保留期 · 当前来源：{settings?.sources.usage_retention_days === "database" ? "数据库" : "环境配置"}</small>
              </label>
              <div className="system-settings-actions">
                <Button variant="primary" type="submit" disabled={role !== "admin" || settingsSaving}>
                  {settingsSaving ? "保存中…" : "保存设置"}
                </Button>
                {role !== "admin" && <span className="secondary-text">当前角色仅可查看</span>}
              </div>
            </form>
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
