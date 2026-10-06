import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import * as echarts from "echarts/core";
import type { EChartsCoreOption } from "echarts/core";
import { LineChart, PieChart } from "echarts/charts";
import { GridComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { ApiClientError } from "../../api/client";
import { Button, ButtonLink } from "../../app/controls/Button";
import { Select } from "../../app/controls/Select";
import { fetchAccounts, type AccountRecord } from "../accounts/accountsApi";
import { fetchRoutes, type ModelRoute } from "../routes/routesApi";
import { fetchSystemMetrics, type SystemMetrics } from "../system/systemApi";
import { fetchUsageRows, type ChannelOverview, type UsageRow } from "../usage/usageApi";
import { fetchOverview, type OverviewPayload, type RecentUsageSeries } from "./overviewApi";
import "./OverviewPage.css";

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

function formatLatency(value: number | null): string {
  if (value === null || value === undefined) return "—";
  return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${Math.round(value)}ms`;
}

function shortDay(value: string): string {
  return new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", timeZone: "UTC" })
    .format(new Date(`${value}T00:00:00Z`));
}

function usageDateRange(days: number): { from: string; to: string } {
  const today = new Date();
  const from = new Date(Date.UTC(
    today.getUTCFullYear(),
    today.getUTCMonth(),
    today.getUTCDate() - days + 1,
  ));
  return {
    from: from.toISOString().slice(0, 10),
    to: today.toISOString().slice(0, 10),
  };
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

function formatMagnitude(value: number): string {
  const absolute = Math.abs(value);
  if (absolute >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(2)}B`;
  if (absolute >= 1_000_000) return `${(value / 1_000_000).toFixed(2)}M`;
  if (absolute >= 1_000) return `${(value / 1_000).toFixed(2)}K`;
  return number(value);
}

function recentAxisTime(value: number): string {
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZone: "UTC",
  }).format(new Date(value)).replace(/\//g, "-");
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

const chartAnimation = {
  animation: true,
  animationDuration: 360,
  animationDurationUpdate: 520,
  animationEasing: "cubicOut" as const,
  animationEasingUpdate: "cubicInOut" as const,
};

const trendRangeOptions = [
  { value: "7", label: "7d" },
  { value: "30", label: "30d" },
  { value: "90", label: "90d" },
];

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
  const chartRef = useRef<ReturnType<typeof echarts.init> | null>(null);

  useEffect(() => {
    const element = elementRef.current;
    if (!element) return undefined;
    const chart = echarts.init(element, undefined, { renderer: "canvas" });
    chartRef.current = chart;
    const resize = () => chart.resize();
    window.addEventListener("resize", resize);
    const observer = new ResizeObserver(resize);
    observer.observe(element);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", resize);
      chart.dispose();
      chartRef.current = null;
    };
  }, []);

  useEffect(() => {
    chartRef.current?.setOption(option, true);
  }, [option]);

  return <div ref={elementRef} className={className} role="img" aria-label={ariaLabel} />;
}

export function OverviewPage() {
  const [overview, setOverview] = useState<OverviewPayload | null>(null);
  const [metrics, setMetrics] = useState<SystemMetrics | null>(null);
  const [accounts, setAccounts] = useState<AccountRecord[]>([]);
  const [routes, setRoutes] = useState<ModelRoute[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [trendDays, setTrendDays] = useState(30);
  const [refreshTick, setRefreshTick] = useState(0);
  const [trendRefreshTick, setTrendRefreshTick] = useState(0);
  const [dailyData, setDailyData] = useState<{ data: UsageRow[]; from: string; to: string } | null>(null);
  const trendDaysRef = useRef(trendDays);
  const initialLoadedRef = useRef(false);
  trendDaysRef.current = trendDays;

  useEffect(() => {
    const controller = new AbortController();
    const initialLoad = !initialLoadedRef.current;
    const requestedTrendDays = trendDaysRef.current;
    if (initialLoad) setLoading(true);
    Promise.allSettled([
      fetchOverview(requestedTrendDays, controller.signal),
      fetchSystemMetrics(1, controller.signal),
      fetchAccounts(1, {}, controller.signal),
      fetchRoutes(controller.signal),
    ]).then(([overviewResult, metricsResult, accountsResult, routesResult]) => {
      if (overviewResult.status === "fulfilled") {
        setOverview(overviewResult.value);
        if (requestedTrendDays === trendDaysRef.current) setDailyData(overviewResult.value.daily);
      }
      if (metricsResult.status === "fulfilled") setMetrics(metricsResult.value);
      if (accountsResult.status === "fulfilled") setAccounts(accountsResult.value.data);
      if (routesResult.status === "fulfilled") setRoutes(routesResult.value.slice(0, 3));
      const failed = [overviewResult, metricsResult, accountsResult, routesResult]
        .find((result) => result.status === "rejected");
      if (failed?.status === "rejected" && !controller.signal.aborted && initialLoad) {
        setError(failed.reason instanceof ApiClientError ? failed.reason.message : "部分运行数据暂时无法读取");
      }
    }).finally(() => {
      if (!controller.signal.aborted && initialLoad) {
        initialLoadedRef.current = true;
        setLoading(false);
        if (requestedTrendDays !== trendDaysRef.current) {
          setTrendRefreshTick((value) => value + 1);
        }
      }
    });
    return () => controller.abort();
  }, [refreshTick]);

  useEffect(() => {
    if (!initialLoadedRef.current) return undefined;
    const controller = new AbortController();
    const requestedTrendDays = trendDays;
    const range = usageDateRange(trendDays);
    fetchUsageRows("daily", requestedTrendDays, controller.signal)
      .then((result) => {
        if (requestedTrendDays !== trendDaysRef.current) return;
        setDailyData({ data: result, from: range.from, to: range.to });
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof ApiClientError ? cause.message : "读取请求趋势失败");
        }
      });
    return () => controller.abort();
  }, [trendDays, trendRefreshTick]);

  useEffect(() => {
    const timer = window.setInterval(() => setRefreshTick((value) => value + 1), 10_000);
    return () => window.clearInterval(timer);
  }, []);

  const channels = overview?.channels ?? [];
  const dailySeries = useMemo(
    () => dailyData ? fillUtcDays(dailyData.data, dailyData.from, dailyData.to) : [],
    [dailyData],
  );
  const dailyChartOption = useMemo<EChartsCoreOption>(() => ({
    ...chartAnimation,
    grid: { left: 8, right: 8, top: 14, bottom: 28, containLabel: false },
    tooltip: { trigger: "axis", valueFormatter: (value: string | number) => `${number(Number(value))} 次` },
    xAxis: {
      type: "category",
      boundaryGap: false,
      data: dailySeries.map((point) => shortDay(point.day)),
      axisLine: { lineStyle: { color: chartPalette.grid } },
      axisTick: { show: false },
      axisLabel: { color: chartPalette.muted, fontSize: 12, interval: Math.max(0, Math.floor(dailySeries.length / 5)) },
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
  const recentUsage = overview?.recent;
  const recentSeries = useMemo(() => recentUsage?.series ?? [], [recentUsage]);
  const hasRecentData = recentSeries.some((series) => series.points.length > 0);
  const recentChartOption = useMemo<EChartsCoreOption>(() => {
    const recentRange = recentUsage
      ? [Date.parse(recentUsage.from), Date.parse(recentUsage.to)] as const
      : [Date.now() - 48 * 60 * 60 * 1000, Date.now()] as const;
    return {
      ...chartAnimation,
      color: ["#3d82f6", "#6f9ff2", "#4f9b7a", "#b7863c", "#b86167", "#7b6fd2"],
      legend: {
        show: recentSeries.length > 0,
        top: 2,
        left: "center",
        icon: "circle",
        itemWidth: 14,
        itemHeight: 14,
        itemGap: 18,
        textStyle: { color: "#52657a", fontSize: 13 },
        data: recentSeries.map((series) => series.key_name),
      },
      grid: { left: 64, right: 18, top: recentSeries.length > 0 ? 42 : 18, bottom: 44, containLabel: true },
      tooltip: hasRecentData ? {
        trigger: "axis",
        axisPointer: { type: "line" },
        valueFormatter: (value: string | number) => formatMagnitude(Number(value)),
      } : { show: false },
      xAxis: {
        type: "time",
        min: recentRange[0],
        max: recentRange[1],
        axisLine: { lineStyle: { color: chartPalette.grid } },
        axisTick: { show: false },
        axisLabel: {
          color: chartPalette.muted,
          fontSize: 12,
          hideOverlap: true,
          formatter: (value: number) => recentAxisTime(value),
        },
        splitLine: { show: true, lineStyle: { color: chartPalette.grid } },
      },
      yAxis: {
        type: "value",
        min: 0,
        splitNumber: 6,
        axisLabel: { color: chartPalette.muted, fontSize: 12, formatter: (value: number) => formatMagnitude(value) },
        axisTick: { show: false },
        axisLine: { show: false },
        splitLine: { lineStyle: { color: chartPalette.grid } },
      },
      series: recentSeries.length > 0 ? recentSeries.map((series: RecentUsageSeries) => ({
        name: series.key_name,
        type: "line",
        smooth: true,
        showSymbol: true,
        symbol: "circle",
        symbolSize: 7,
        connectNulls: false,
        data: series.points.map((point) => [Date.parse(point.ts), point.tokens]),
        lineStyle: { width: 3 },
        itemStyle: { borderWidth: 1, borderColor: "#ffffff" },
        emphasis: { focus: "series" },
      })) : [{
        name: "",
        type: "line",
        data: [[recentRange[0], 0], [recentRange[1], 0]],
        symbol: "none",
        silent: true,
        lineStyle: { color: chartPalette.grid, width: 1, type: "dashed" },
      }],
    };
  }, [hasRecentData, recentSeries, recentUsage]);
  const accountMetrics = metrics?.accounts ?? { total: 0, available: 0 };
  const abnormalAccounts = Math.max(0, accountMetrics.total - accountMetrics.available);
  const successRate = metrics ? Math.max(0, (1 - metrics.error_rate) * 100) : 0;
  const accountStatusCounts = useMemo(
    () => accountStatusLegend.map((item) => ({ ...item, count: accounts.filter((account) => accountState(account).tone === item.tone).length })),
    [accounts],
  );
  const accountChartOption = useMemo<EChartsCoreOption>(() => ({
    ...chartAnimation,
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
          <ButtonLink to="/usage">导出报表</ButtonLink>
          <ButtonLink to="/channels">测试全部渠道</ButtonLink>
          <ButtonLink variant="primary" to="/channels">+ 添加渠道</ButtonLink>
        </div>
      </div>

      {error && <div className="notice notice-error" role="alert"><span>{error}</span><Button variant="secondary" size="sm" onClick={() => setRefreshTick((value) => value + 1)}>重试</Button></div>}

      <section className="overview-kpis" aria-label="运行指标">
        <Metric label="今日请求" value={loading ? "…" : number(metrics?.requests ?? 0)} icon="↗" meta={metrics ? "最近 24h" : "等待数据"} />
        <Metric label="成功率" value={loading ? "…" : `${successRate.toFixed(2)}%`} icon="✓" meta="最近 24h" />
        <Metric label="P95 延迟" value={loading ? "…" : formatLatency(metrics?.p95_latency_ms ?? 0)} icon="◷" meta="最近 24h" />
        <Metric label="可用账号" value={loading ? "…" : `${number(accountMetrics.available)} / ${number(accountMetrics.total)}`} icon="◎" meta={accountMetrics.total ? `${((accountMetrics.available / accountMetrics.total) * 100).toFixed(1)}% Ready / Busy` : "暂无账号"} />
        <Metric label="异常账号" value={loading ? "…" : number(abnormalAccounts)} icon="!" meta="需要关注的运行态" tone={abnormalAccounts > 0 ? "warning" : undefined} />
      </section>

      <section className="overview-grid overview-top-grid">
        <article className="overview-panel chart-panel">
          <PanelHead
            title="请求量趋势"
            desc={`最近 ${trendDays} 天 · UTC 记账日`}
            action={(
              <Select
                value={String(trendDays)}
                ariaLabel="请求量趋势日期范围"
                options={trendRangeOptions}
                onChange={(value) => setTrendDays(Number(value))}
                className="overview-range-select"
              />
            )}
          />
          <div className="panel-content">
            {loading ? <div className="table-state">正在读取请求趋势…</div> : dailySeries.length === 0 ? <div className="table-state">最近 {trendDays} 天暂无请求记录</div> : (
              <EChart option={dailyChartOption} className="overview-chart" ariaLabel={`最近 ${trendDays} 天请求量趋势`} />
            )}
          </div>
        </article>

        <article className="overview-panel health-panel">
          <PanelHead title="渠道健康" desc="实时聚合 · 本地网关运行态" action={<ButtonLink size="sm" to="/channels">查看全部</ButtonLink>} />
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
          <PanelHead title="账号池" desc="最近活跃账号与额度状态" action={<ButtonLink size="sm" to="/accounts">查看全部</ButtonLink>} />
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
          <PanelHead title="路由规则" desc="跨渠道降级链 · 当前配置" action={<ButtonLink size="sm" to="/routes">管理路由</ButtonLink>} />
          <div className="panel-content route-list">
            {loading ? <div className="table-state">正在读取路由…</div> : routes.length === 0 ? <div className="table-state">还没有路由规则</div> : routes.map((route) => <div className="route-card" key={route.alias}><div className="route-card-head"><strong>{route.alias}</strong><span>{route.strategy === "priority" ? "Priority" : route.strategy} · {route.targets.length} targets</span></div><div className="route-flow">{route.targets.slice(0, 3).map((target, index) => <span className="route-target" key={`${target.channel}-${target.model}`}><b>{index + 1}</b><strong>{target.channel}</strong><small>{target.model}</small></span>)}</div></div>)}
          </div>
        </article>
      </section>

      <section className="overview-panel recent-panel">
        <header className="recent-panel-head"><h2>最近使用 (Top 12)</h2></header>
        <div className="panel-content recent-visual-content">
          {loading ? <div className="table-state">正在读取请求日志…</div> : (
            <>
              <div className="recent-chart-shell">
                <EChart option={recentChartOption} className="recent-chart" ariaLabel={hasRecentData ? "最近使用 Top 12 的 token 用量趋势" : "最近使用 Top 12 暂无真实用量记录"} />
              </div>
              {!hasRecentData && <div className="recent-empty-note" role="status">暂无真实用量数据</div>}
            </>
          )}
        </div>
      </section>
      <div className="footer-note">All2API · 统一入口 :8888 · Neumorphism × Data Ops UI</div>
    </main>
  );
}

function PanelHead({ title, desc, action }: { title: string; desc: string; action?: ReactNode }) {
  return <header className="overview-panel-head"><div><h2>{title}</h2><p>{desc}</p></div>{action}</header>;
}

function Metric({ label, value, icon, meta, tone }: { label: string; value: string; icon: string; meta: string; tone?: string }) {
  return <div className="overview-kpi"><div className="overview-kpi-top"><span>{label}</span><b className={tone ?? ""}>{icon}</b></div><strong>{value}</strong><small className={tone ?? ""}>{meta}</small></div>;
}
