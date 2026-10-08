import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { ApiClientError, apiClient, onUnauthorized } from "../api/client";
import { Button } from "./controls/Button";
import { LoginPage } from "../features/auth/LoginPage";

type AuthState =
  | { status: "checking" }
  | { status: "signed-out"; error?: string }
  | { status: "signed-in"; username: string; role: "admin" | "viewer" }
  | { status: "unavailable"; error: string };

const searchablePages = [
  { label: "运行总览", path: "/", keywords: ["overview", "dashboard"] },
  { label: "渠道", path: "/channels", keywords: ["channel"] },
  { label: "账号池", path: "/accounts", keywords: ["account", "账号"] },
  { label: "模型广场", path: "/models", keywords: ["model", "模型"] },
  { label: "素材库", path: "/media", keywords: ["media", "asset", "素材", "图库"] },
  { label: "路由规则", path: "/routes", keywords: ["route", "路由"] },
  { label: "网关密钥", path: "/keys", keywords: ["key", "密钥"] },
  { label: "请求日志", path: "/logs", keywords: ["request", "log", "请求"] },
  { label: "审计日志", path: "/audit-logs", keywords: ["audit", "审计"] },
  { label: "系统健康", path: "/system", keywords: ["system", "health", "系统"] },
  { label: "用量统计", path: "/usage", keywords: ["usage", "统计"] },
  { label: "调试台", path: "/playground", keywords: ["playground", "debug", "调试"] },
  { label: "用户管理", path: "/users", keywords: ["users", "user", "用户"] },
];

export function AppShell() {
  const [auth, setAuth] = useState<AuthState>({ status: "checking" });
  const [theme, setTheme] = useState<"light" | "dark">(() =>
    window.localStorage.getItem("all2api-theme") === "dark" ? "dark" : "light",
  );
  const [search, setSearch] = useState("");
  const location = useLocation();
  const navigate = useNavigate();
  const pageTitle: Record<string, string> = {
    "/": "运行总览",
    "/channels": "渠道",
    "/accounts": "账号池",
    "/models": "模型广场",
    "/media": "素材库",
    "/routes": "路由规则",
    "/keys": "网关密钥",
    "/logs": "请求日志",
    "/audit-logs": "审计日志",
    "/system": "系统健康",
    "/usage": "用量统计",
    "/playground": "调试台",
    "/users": "用户管理",
  };

  useEffect(() => onUnauthorized(() => setAuth({ status: "signed-out" })), []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
    window.localStorage.setItem("all2api-theme", theme);
  }, [theme]);

  useEffect(() => {
    const controller = new AbortController();
    apiClient
      .get<{ data: { user: { username: string; role: "admin" | "viewer" } } }>("/auth/session", {
        signal: controller.signal,
      })
      .then((response) => {
        setAuth({
          status: "signed-in",
          username: response.data.data.user.username,
          role: response.data.data.user.role,
        });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        if (cause instanceof ApiClientError && cause.status === 401) {
          setAuth({ status: "signed-out" });
          return;
        }
        setAuth({
          status: "unavailable",
          error: cause instanceof Error ? cause.message : "无法连接管理服务",
        });
      });
    return () => controller.abort();
  }, []);

  if (auth.status === "checking") {
    return <main className="auth-loading" aria-live="polite">正在检查登录状态…</main>;
  }
  if (auth.status === "signed-out") {
    return (
      <LoginPage
        initialError={auth.error}
        onAuthenticated={(username, role) => setAuth({ status: "signed-in", username, role })}
      />
    );
  }
  if (auth.status === "unavailable") {
    return (
      <main className="auth-loading" role="alert">
        <p>管理服务暂不可用</p>
        <p>{auth.error}</p>
        <Button variant="secondary" onClick={() => setAuth({ status: "checking" })}>
          重试
        </Button>
      </main>
    );
  }

  async function logout() {
    try {
      await apiClient.post("/auth/logout");
    } finally {
      setAuth({ status: "signed-out" });
    }
  }

  function submitSearch(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const query = search.trim();
    if (!query) return;
    const normalized = query.toLocaleLowerCase("zh-CN");
    const page = searchablePages.find((item) =>
      item.label.toLocaleLowerCase("zh-CN").includes(normalized)
      || item.keywords.some((keyword) => keyword.toLocaleLowerCase("zh-CN").includes(normalized)),
    );
    navigate(page?.path ?? `/logs?request_id=${encodeURIComponent(query)}`);
    setSearch("");
  }

  return (
    <div className="app-frame">
      <header className="app-header">
        <div className="header-context">
          <span className="header-context-root">控制台</span>
          <span className="header-context-divider" aria-hidden="true">/</span>
          <span className="header-context-current">{pageTitle[location.pathname] ?? "All2API"}</span>
        </div>
        <div className="header-spacer" />
        <form className="header-search-form" role="search" onSubmit={submitSearch}>
          <input
            className="header-search"
            type="search"
            aria-label="搜索页面或请求 ID"
            placeholder="搜索页面或请求 ID…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </form>
        <Button
          variant="icon"
          className="header-icon-button theme-toggle"
          aria-label={theme === "dark" ? "切换到浅色主题" : "切换到深色主题"}
          title={theme === "dark" ? "切换到浅色主题" : "切换到深色主题"}
          onClick={() => setTheme((value) => value === "dark" ? "light" : "dark")}
        >
          {theme === "dark" ? "☀" : "☼"}
        </Button>
        <Button variant="icon" className="header-icon-button" aria-label="通知" title="通知">⌁</Button>
        <div className="app-account">
          <span className="app-user-mark" aria-hidden="true">{auth.username.slice(0, 1).toUpperCase()}</span>
          <span>{auth.username}</span>
          <Button variant="secondary" size="sm" onClick={logout}>退出</Button>
        </div>
      </header>
      <div className="app-layout">
        <aside className="app-sidebar">
          <div className="sidebar-brand">
            <span className="brand-mark" aria-hidden="true"><img src="/logo.svg" alt="" /></span>
            <span className="brand-copy"><strong>All2API</strong><small>统一 API 网关控制台</small></span>
          </div>
          <nav aria-label="主导航">
          <div className="nav-group">
              <span className="side-label">概览</span>
              <NavLink end to="/" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-overview" aria-hidden="true" />运行总览
              </NavLink>
            </div>
            <div className="nav-group">
              <span className="side-label">资源</span>
              <NavLink to="/channels" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-channels" aria-hidden="true" />渠道
              </NavLink>
              <NavLink to="/accounts" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-accounts" aria-hidden="true" />账号池
              </NavLink>
              <NavLink to="/models" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-models" aria-hidden="true" />模型广场
              </NavLink>
              <NavLink to="/media" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-media" aria-hidden="true" />素材库
              </NavLink>
              <NavLink to="/routes" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-routes" aria-hidden="true" />路由规则
              </NavLink>
              <NavLink to="/keys" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-keys" aria-hidden="true" />网关密钥
              </NavLink>
            </div>
            <div className="nav-group">
              <span className="side-label">观测</span>
              <NavLink to="/logs" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-logs" aria-hidden="true" />请求日志
              </NavLink>
              <NavLink to="/audit-logs" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-audit" aria-hidden="true" />审计日志
              </NavLink>
              <NavLink to="/system" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-health" aria-hidden="true" />健康检查
              </NavLink>
              <NavLink to="/usage" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-usage" aria-hidden="true" />用量统计
              </NavLink>
              <NavLink to="/playground" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-logs" aria-hidden="true" />调试台
              </NavLink>
            </div>
            <div className="nav-group">
              <span className="side-label">管理</span>
              <NavLink to="/users" className={({ isActive }) => isActive ? "side-link active" : "side-link"}>
                <span className="side-icon side-icon-keys" aria-hidden="true" />用户管理
              </NavLink>
            </div>
          </nav>
          <section className="sidebar-status" aria-label="网关状态">
            <strong>网关状态</strong>
            <p>管理服务连接正常，运行数据持续更新。</p>
            <span><i aria-hidden="true" /> Gateway Healthy</span>
          </section>
        </aside>
        <div className={`page-stage ${location.pathname === "/" ? "page-stage-overview" : location.pathname === "/system" ? "page-stage-system" : "page-stage-data"}`}>
          <Outlet context={{ role: auth.role }} />
        </div>
      </div>
    </div>
  );
}
