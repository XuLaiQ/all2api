import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import * as echarts from "echarts/core";
import type { EChartsCoreOption } from "echarts/core";
import { LineChart, PieChart } from "echarts/charts";
import { GridComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { Link } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { fetchAccounts, type AccountRecord } from "../accounts/accountsApi";
import { fetchLogs, type RequestLog } from "../logs/logsApi";
import { fetchRoutes, type ModelRoute } from "../routes/routesApi";
import { fetchSystemMetrics, type SystemMetrics } from "../system/systemApi";
import type { ChannelOverview, UsageRow } from "../usage/usageApi";
import { fetchOverview, type OverviewPayload } from "./overviewApi";

echarts.use([LineChart, PieChart, GridComponent, TooltipComponent, CanvasRenderer]);

function number(value: number): string {
  return new Intl.NumberFormat("zh-CN").format(value);
}

function decimal(value: number, digits = 2): string {
  return new Intl.NumberFormat("zh-CN", { maximumFractionDigits: digits }).format(value);
}

function channelState(channel: ChannelOverview): { label: string; tone: string } {
  if (!channel.enabled) return { label: "未配置", tone: "neutral" };
  if (channel.runtime.state === "breaker_open" || channel.state === "breaker_open") return { label: "熔断中", tone: "danger" };
  if (channel.runtime.state === "cooldown" || channel.state === "cooldown") return { label: "冷却中", tone: "warning" };
  return { label: "Healthy", tone: "success" };
}

function accountState(account: AccountRecord): { label: string; tone: string } {
  if (!account.enabled) return { label: "Disabled", tone: "neutral" };
  if (["needLogin", "captcha", "error", "expired"].includes(account.status)) return { label: "Need Login", tone: "danger" };
  if (["cooldown", "limited"].includes(account.status)) return { label: "Limited", tone: "warning" };
  if (account.status === "busy") return { label: "Busy", tone: "info" };
  return { label: "Ready", tone: "success" };
}

function logTone(status: number): string {
  if (status >= 200 && status < 300) return "success";
  if (status >= 400) return "danger";
  return "warning";
}

function formatLatency(value: number | null): string {
  if (value === null || value === undefined) return "—";
  return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${Math.round(value)}ms`;
}

function shortDay(value: string): string {
  return new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", timeZone: "UTC" })
    .format(new Date(`${value}T00:00:00Z`));
}

type DailyPoint = { day: string; requests: number };

function fillUtcDays(rows: UsageRow[], from: string, to: string): DailyPoint[] {
  const totals = new Map(rows.map((row) => [row.day ?? "", row.requests]));
  const first = new Date(`${from}T00:00:00Z`);
  const last = new Date(`${to}T00:00:00Z`);
  const days: DailyPoint[] = [];
  for (const date = new Date(first); date <= last; date.setUTCDate(date.getUTCDate() + 1)) {
    const day = date.toISOString().slice(0, 10);
    days.push({ day, requests: totals.get(day) ?? 0 });
  }
  return days;
}

function recentTime(value: string): string {
  return new Intl.DateTimeFormat("zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
    timeZone: "UTC",
  }).format(new Date(value));
}

const accountStatusLegend = [
  { tone: "success", label: "Ready" },
  { tone: "info", label: "Busy" },
  { tone: "warning", label: "Limited" },
  { tone: "danger", label: "Need Login" },
];

const chartPalette = {
  primary: "#4b5fd6",
  primarySoft: "#7588ee",
  success: "#4f9b7a",
  info: "#4f82b9",
  warning: "#b7863c",
  danger: "#b86167",
  muted: "#8192a5",
  grid: "rgba(129, 146, 165, .20)",
};

function EChart({
  option,
  className,
  ariaLabel,
}: {
  option: EChartsCoreOption;
  className: string;
  ariaLabel: string;
}) {
  const elementRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const element = elementRef.current;
    if (!element) return undefined;
    const chart = echarts.init(element, undefined, { renderer: "canvas" });
    chart.setOption(option, true);
    const resize = () => chart.resize();
    window.addEventListener("resize", resize);
    const observer = new ResizeObserver(resize);
    observer.observe(element);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", resize);
      chart.dispose();
    };
  }, [option]);

  return <div ref={elementRef} className={className} role="img" aria-label={ariaLabel} />;
}

export function OverviewPage() {
  const [overview, setOverview] = useState<OverviewPayload | null>(null);
  const [metrics, setMetrics] = useState<SystemMetrics | null>(null);
  const [accounts, setAccounts] = useState<AccountRecord[]>([]);
  const [routes, setRoutes] = useState<ModelRoute[]>([]);
  const [logs, setLogs] = useState<RequestLog[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    Promise.allSettled([
      fetchOverview(30, controller.signal),
      fetchSystemMetrics(1, controller.signal),
      fetchAccounts(1, {}, controller.signal),
      fetchRoutes(controller.signal),
      fetchLogs(1, {}, controller.signal),
    ]).then(([overviewResult, metricsResult, accountsResult, routesResult, logsResult]) => {
      if (overviewResult.status === "fulfilled") setOverview(overviewResult.value);
      if (metricsResult.status === "fulfilled") setMetrics(metricsResult.value);
      if (accountsResult.status === "fulfilled") setAccounts(accountsResult.value.data);
      if (routesResult.status === "fulfilled") setRoutes(routesResult.value.slice(0, 3));
      if (logsResult.status === "fulfilled") setLogs(logsResult.value.data.slice(0, 12));
      const failed = [overviewResult, metricsResult, accountsResult, routesResult, logsResult]
        .find((result) => result.status === "rejected");
      if (failed?.status === "rejected" && !controller.signal.aborted) {
        setError(failed.reason instanceof ApiClientError ? failed.reason.message : "部分运行数据暂时无法读取");
      }
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [retry]);

  const summary = overview?.summary;
  const channels = overview?.channels ?? [];
  const dailySeries = useMemo(() => summary ? fillUtcDays(overview?.daily.data ?? [], summary.from, summary.to) : [], [overview, summary]);
  const dailyChartOption = useMemo<EChartsCoreOption>(() => ({
    animation: false,
    grid: { left: 8, right: 8, top: 14, bottom: 28, containLabel: false },
    tooltip: { trigger: "axis", valueFormatter: (value: string | number) => `${number(Number(value))} 次` },
    xAxis: {
      type: "category",
      boundaryGap: false,
      data: dailySeries.map((point) => shortDay(point.day)),
      axisLine: { lineStyle: { color: chartPalette.grid } },
      axisTick: { show: false },
      axisLabel: { color: chartPalette.muted, fontSize: 10, interval: Math.max(0, Math.floor(dailySeries.length / 5)) },
    },
    yAxis: {
      type: "value",
      axisLabel: { show: false },
      axisTick: { show: false },
      axisLine: { show: false },
      splitLine: { lineStyle: { color: chartPalette.grid } },
    },
    series: [{
      type: "line",
      smooth: true,
      symbol: "circle",
      symbolSize: 6,
      data: dailySeries.map((point) => point.requests),
      lineStyle: { color: chartPalette.primary, width: 3 },
      itemStyle: { color: chartPalette.primary },
      areaStyle: { color: chartPalette.primarySoft, opacity: .22 },
    }],
  }), [dailySeries]);
  const recentLogs = useMemo(() => [...logs].reverse(), [logs]);
  const recentPoints = useMemo(() => recentLogs.map((log) => ({
    log,
    label: recentTime(log.ts),
    value: Math.max(0, log.latency_ms),
  })), [recentLogs]);
  const recentPeak = Math.max(0, ...recentPoints.map((point) => point.value));
  const recentChartOption = useMemo<EChartsCoreOption>(() => ({
    animation: false,
    grid: { left: 8, right: 8, top: 14, bottom: 28, containLabel: false },
    tooltip: { trigger: "axis", valueFormatter: (value: string | number) => formatLatency(Number(value)) },
    xAxis: {
      type: "category",
      boundaryGap: false,
      data: recentPoints.map((point) => point.label),
      axisLine: { lineStyle: { color: chartPalette.grid } },
      axisTick: { show: false },
      axisLabel: { color: chartPalette.muted, fontSize: 10, interval: Math.max(0, Math.floor(recentPoints.length / 5)) },
    },
    yAxis: {
      type: "value",
      axisLabel: { color: chartPalette.muted, fontSize: 10, formatter: (value: number) => formatLatency(value) },
      axisTick: { show: false },
      axisLine: { show: false },
      splitLine: { lineStyle: { color: chartPalette.grid } },
    },
    series: [{
      type: "line",
      smooth: true,
      symbol: "circle",
      symbolSize: 8,
      data: recentPoints.map((point) => ({
        value: point.value,
        itemStyle: { color: logTone(point.log.status) === "danger" ? chartPalette.danger : chartPalette.primary },
      })),
      lineStyle: { color: chartPalette.primary, width: 3 },
      itemStyle: { color: chartPalette.primary },
      areaStyle: { color: chartPalette.primarySoft, opacity: .2 },
    }],
  }), [recentPoints]);
  const accountMetrics = metrics?.accounts ?? { total: 0, available: 0 };
  const abnormalAccounts = Math.max(0, accountMetrics.total - accountMetrics.available);
  const successRate = metrics ? Math.max(0, (1 - metrics.error_rate) * 100) : 0;
  const accountStatusCounts = useMemo(
    () => accountStatusLegend.map((item) => ({ ...item, count: accounts.filter((account) => accountState(account).tone === item.tone).length })),
    [accounts],
  );
  const accountChartOption = useMemo<EChartsCoreOption>(() => ({
    animation: false,
    tooltip: { trigger: "item", formatter: "{b}: {c} ({d}%)" },
    series: [{
      type: "pie",
      radius: ["58%", "82%"],
      center: ["50%", "50%"],
      label: { show: false },
      labelLine: { show: false },
      data: accountStatusCounts.some((item) => item.count > 0)
        ? accountStatusCounts.map((item) => ({
          name: item.label,
          value: item.count,
          itemStyle: { color: chartPalette[item.tone as keyof typeof chartPalette] ?? chartPalette.muted },
        }))
        : [{ name: "暂无账号", value: 1, itemStyle: { color: chartPalette.grid } }],
    }],
  }), [accountStatusCounts]);

  return (
    <main className="page-content overview-page">
      <div className="page-heading overview-heading">
        <div>
          <span className="page-eyebrow">DATA OPS CONSOLE</span>
          <h1>运行总览</h1>
          <p>统一查看渠道健康、账号池状态、路由表现和请求指标。</p>
        </div>
        <div className="page-actions">
          <Link className="secondary-action-button" to="/usage">导出报表</Link>
          <Link className="secondary-action-button" to="/channels">测试全部渠道</Link>
          <Link className="key-primary-action" to="/channels">+ 添加渠道</Link>
        </div>
      </div>

      {error && <div className="notice notice-error" role="alert"><span>{error}</span><button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button></div>}

      <section className="overview-kpis" aria-label="运行指标">
        <Metric label="今日请求" value={loading ? "…" : number(metrics?.requests ?? 0)} icon="↗" meta={metrics ? "最近 24h" : "等待数据"} />
        <Metric label="成功率" value={loading ? "…" : `${successRate.toFixed(2)}%`} icon="✓" meta="最近 24h" />
        <Metric label="P95 延迟" value={loading ? "…" : formatLatency(metrics?.p95_latency_ms ?? 0)} icon="◷" meta="最近 24h" />
        <Metric label="可用账号" value={loading ? "…" : `${number(accountMetrics.available)} / ${number(accountMetrics.total)}`} icon="◎" meta={accountMetrics.total ? `${((accountMetrics.available / accountMetrics.total) * 100).toFixed(1)}% Ready / Busy` : "暂无账号"} />
        <Metric label="异常账号" value={loading ? "…" : number(abnormalAccounts)} icon="!" meta="需要关注的运行态" tone={abnormalAccounts > 0 ? "warning" : undefined} />
      </section>

      <section className="overview-grid overview-top-grid">
        <article className="overview-panel chart-panel">
          <PanelHead title="请求量趋势" desc="最近 30 天 · UTC 记账日" action={<Link className="secondary-action-button compact-action" to="/usage">30d⌄</Link>} />
          <div className="panel-content">
            {loading ? <div className="table-state">正在读取请求趋势…</div> : dailySeries.length === 0 ? <div className="table-state">最近 30 天暂无请求记录</div> : (
              <EChart option={dailyChartOption} className="overview-chart" ariaLabel="最近 30 天请求量趋势" />
            )}
          </div>
        </article>

        <article className="overview-panel health-panel">
          <PanelHead title="渠道健康" desc="实时聚合 · 本地网关运行态" action={<Link className="secondary-action-button compact-action" to="/channels">查看全部</Link>} />
          <div className="panel-content health-list">
            {loading ? <div className="table-state">正在读取渠道…</div> : channels.length === 0 ? <div className="table-state">没有已注册渠道</div> : channels.slice(0, 4).map((channel) => {
              const state = channelState(channel);
              return <div className="health-row" key={channel.slug}><div className="health-main"><span className={`health-dot ${state.tone}`} /><div><strong>{channel.name}</strong><small>{channel.adapter} · {channel.slug}</small></div></div><span className={`status-pill ${state.tone}`}>{state.label}</span></div>;
            })}
          </div>
        </article>
      </section>

      <section className="overview-grid overview-middle-grid">
        <article className="overview-panel table-panel">
          <PanelHead title="账号池" desc="最近活跃账号与额度状态" action={<Link className="secondary-action-button compact-action" to="/accounts">查看全部</Link>} />
          <div className="panel-content account-visual-content">
            {loading ? <div className="table-state">正在读取账号…</div> : (
              <div className="account-visual-layout">
                <div className="account-health-visual">
                  <div className="account-donut-chart-wrap">
                    <EChart option={accountChartOption} className="account-donut-chart" ariaLabel={`账号池状态分布，共 ${accountMetrics.total || accounts.length} 个账号`} />
                    <div className="account-donut-center"><strong>{number(accountMetrics.total || accounts.length)}</strong><span>accounts</span></div>
                  </div>
                  <div className="account-legend">
                    {accountStatusCounts.map((item) => <div className="account-legend-item" key={item.tone}><span className={`legend-dot ${item.tone}`} /><span>{item.label}</span><strong>{item.count}</strong></div>)}
                  </div>
                </div>
                <div className="account-card-grid">
                  {accounts.length === 0 ? <div className="account-empty-card">暂无账号快照</div> : accounts.slice(0, 4).map((account) => {
                    const state = accountState(account);
                    const quota = account.quota_total > 0 ? Math.min(100, Math.max(0, account.quota_used / account.quota_total * 100)) : 0;
                    const success = account.gateway_runtime.success_count + account.gateway_runtime.fail_count > 0 ? account.gateway_runtime.success_count / (account.gateway_runtime.success_count + account.gateway_runtime.fail_count) * 100 : null;
                    return <article className="account-visual-card" key={account.id}>
                      <div className="account-card-head"><div className="account-avatar">{(account.name || "A").slice(0, 1).toUpperCase()}</div><div><strong>{account.name || "Unnamed"}</strong><small>{account.channel}</small></div><span className={`status-pill ${state.tone}`}><i />{state.label}</span></div>
                      <div className="account-card-foot"><div className="quota-bar"><span style={{ width: `${quota}%` }} /></div><small>{account.quota_total > 0 ? `${decimal(quota, 0)}% quota` : "Quota n/a"}</small><b>{success === null ? "—" : `${success.toFixed(1)}%`}</b></div>
                    </article>;
                  })}
                </div>
              </div>
            )}
          </div>
        </article>

        <article className="overview-panel routes-panel">
          <PanelHead title="路由规则" desc="跨渠道降级链 · 当前配置" action={<Link className="secondary-action-button compact-action" to="/routes">管理路由</Link>} />
          <div className="panel-content route-list">
            {loading ? <div className="table-state">正在读取路由…</div> : routes.length === 0 ? <div className="table-state">还没有路由规则</div> : routes.map((route) => <div className="route-card" key={route.alias}><div className="route-card-head"><strong>{route.alias}</strong><span>{route.strategy === "priority" ? "Priority" : route.strategy} · {route.targets.length} targets</span></div><div className="route-flow">{route.targets.slice(0, 3).map((target, index) => <span className="route-target" key={`${target.channel}-${target.model}`}><b>{index + 1}</b><strong>{target.channel}</strong><small>{target.model}</small></span>)}</div></div>)}
          </div>
        </article>
      </section>

      <section className="overview-panel recent-panel">
        <PanelHead title="最近使用" desc="最近 12 条请求 · 延迟走势" action={<Link className="secondary-action-button compact-action" to="/logs">打开请求日志</Link>} />
        <div className="panel-content recent-visual-content">
          {loading ? <div className="table-state">正在读取请求日志…</div> : (
            <>
              <div className="recent-chart-legend"><span className="legend-line" />Latency <strong>{recentPoints.length > 0 ? `${formatLatency(recentPeak)} peak` : "等待数据"}</strong></div>
              <div className="recent-chart-shell">
                <EChart option={recentChartOption} className="recent-chart" ariaLabel="最近 12 条请求的延迟走势" />
                {recentPoints.length === 0 && <span className="recent-empty-overlay">暂无请求记录</span>}
              </div>
              {recentPoints.length > 0 ? <div className="recent-request-strip">{recentPoints.slice(-4).map((point) => <div className="recent-request-chip" key={point.log.id}><span className={`health-dot ${logTone(point.log.status)}`} /><div><strong>{point.log.model ?? "unknown"}</strong><small>{point.label} · {formatLatency(point.value)}</small></div></div>)}</div> : <div className="recent-empty-note">数据接入后显示最近 12 条请求</div>}
            </>
          )}
        </div>
      </section>
      <div className="footer-note">All2API · 统一入口 :8080 · Neumorphism × Data Ops UI</div>
    </main>
  );
}

function PanelHead({ title, desc, action }: { title: string; desc: string; action?: ReactNode }) {
  return <header className="overview-panel-head"><div><h2>{title}</h2><p>{desc}</p></div>{action}</header>;
}

function Metric({ label, value, icon, meta, tone }: { label: string; value: string; icon: string; meta: string; tone?: string }) {
  return <div className="overview-kpi"><div className="overview-kpi-top"><span>{label}</span><b className={tone ?? ""}>{icon}</b></div><strong>{value}</strong><small className={tone ?? ""}>{meta}</small></div>;
}
