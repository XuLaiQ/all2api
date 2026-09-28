import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type FormEvent,
  type KeyboardEvent,
  type PointerEvent,
} from "react";
import Particles, { ParticlesProvider } from "@tsparticles/react";
import type { Engine, ISourceOptions } from "@tsparticles/engine";
import { loadSlim } from "@tsparticles/slim";
import { ArrowUpRight, Eye, EyeOff, LockKeyhole, ShieldCheck, UserRound } from "lucide-react";
import { ApiClientError, apiClient } from "../../api/client";

interface LoginPageProps {
  onAuthenticated: (username: string, role: "admin" | "viewer") => void;
  initialError?: string;
}

/* ---------------------------------- icons --------------------------------- */

function UserIcon() {
  return <UserRound className="auth-input-icon" size={17} strokeWidth={1.8} aria-hidden="true" />;
}

function LockIcon() {
  return <LockKeyhole className="auth-input-icon" size={17} strokeWidth={1.8} aria-hidden="true" />;
}

function EyeToggle() {
  return (
    <span className="auth-eye" aria-hidden="true">
      <Eye
        className="auth-eye-icon auth-eye-open"
        size={18}
        strokeWidth={1.8}
      >
      </Eye>
      <EyeOff
        className="auth-eye-icon auth-eye-closed"
        size={18}
        strokeWidth={1.8}
      >
      </EyeOff>
    </span>
  );
}

function ArrowIcon() {
  return <ArrowUpRight size={18} strokeWidth={2.1} aria-hidden="true" />;
}

function ShieldIcon() {
  return <ShieldCheck size={13} strokeWidth={1.8} aria-hidden="true" />;
}

/* ------------------------------- backdrop --------------------------------- */

function AuthAurora() {
  return (
    <div className="auth-aurora" aria-hidden="true">
      <span className="auth-aurora-orb auth-aurora-orb--a" />
      <span className="auth-aurora-orb auth-aurora-orb--b" />
      <span className="auth-aurora-orb auth-aurora-orb--c" />
    </div>
  );
}

function AuthParticleField() {
  const initParticles = useCallback(async (engine: Engine) => {
    await loadSlim(engine);
  }, []);

  const options = useMemo<ISourceOptions>(() => ({
    autoPlay: !window.matchMedia("(prefers-reduced-motion: reduce)").matches,
    background: { color: { value: "transparent" } },
    detectRetina: true,
    fpsLimit: 45,
    fullScreen: { enable: false },
    interactivity: {
      detectsOn: "window",
      events: {
        onHover: { enable: true, mode: "attract" },
        resize: { enable: true },
      },
      modes: {
        attract: { distance: 170, duration: 0.35, factor: 1.25, speed: 1.2 },
      },
    },
    particles: {
      color: { value: "#4b5fd6" },
      links: {
        color: "#4b5fd6",
        distance: 165,
        enable: true,
        opacity: 0.34,
        width: 1,
      },
      move: {
        direction: "top-right",
        enable: !window.matchMedia("(prefers-reduced-motion: reduce)").matches,
        outModes: { default: "bounce" },
        random: false,
        speed: { min: 0.4, max: 0.85 },
        straight: false,
      },
      number: {
        density: { enable: true, height: 900, width: 1200 },
        limit: { mode: "delete", value: 88 },
        value: 72,
      },
      opacity: { value: { min: 0.42, max: 0.82 } },
      reduceDuplicates: true,
      shape: { type: "circle" },
      size: { value: { min: 1.2, max: 3 } },
    },
    pauseOnBlur: true,
    pauseOnOutsideViewport: true,
  }), []);

  return (
    <div className="auth-particle-layer" aria-hidden="true">
      <ParticlesProvider init={initParticles}>
        <Particles id="auth-particles" className="auth-particle-field" options={options} />
      </ParticlesProvider>
    </div>
  );
}

/* --------------------------------- page ----------------------------------- */

export function LoginPage({ onAuthenticated, initialError }: LoginPageProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [capsLock, setCapsLock] = useState(false);
  const [error, setError] = useState(initialError ?? "");
  const [submitting, setSubmitting] = useState(false);

  const panelRef = useRef<HTMLElement>(null);
  const tiltFrame = useRef<number | null>(null);
  const [tilt, setTilt] = useState({ rx: 0, ry: 0, mx: 50, my: 50 });

  const tiltEnabled = useMemo(
    () =>
      typeof window !== "undefined" &&
      window.matchMedia("(pointer: fine)").matches &&
      !window.matchMedia("(prefers-reduced-motion: reduce)").matches,
    [],
  );

  useEffect(() => {
    return () => {
      if (tiltFrame.current !== null) window.cancelAnimationFrame(tiltFrame.current);
    };
  }, []);

  const handlePanelPointerMove = useCallback(
    (event: PointerEvent<HTMLElement>) => {
      if (!tiltEnabled) return;
      const node = panelRef.current;
      if (!node) return;
      if (tiltFrame.current !== null) window.cancelAnimationFrame(tiltFrame.current);
      const rect = node.getBoundingClientRect();
      const px = (event.clientX - rect.left) / rect.width;
      const py = (event.clientY - rect.top) / rect.height;
      tiltFrame.current = window.requestAnimationFrame(() => {
        tiltFrame.current = null;
        setTilt({
          rx: (0.5 - py) * 7,
          ry: (px - 0.5) * 7,
          mx: px * 100,
          my: py * 100,
        });
      });
    },
    [tiltEnabled],
  );

  const handlePanelPointerLeave = useCallback(() => {
    if (tiltFrame.current !== null) {
      window.cancelAnimationFrame(tiltFrame.current);
      tiltFrame.current = null;
    }
    setTilt({ rx: 0, ry: 0, mx: 50, my: 50 });
  }, []);

  const trackCapsLock = useCallback((event: KeyboardEvent<HTMLInputElement>) => {
    if (typeof event.getModifierState === "function") {
      setCapsLock(event.getModifierState("CapsLock"));
    }
  }, []);

  const handleSubmitPointerMove = useCallback((event: PointerEvent<HTMLButtonElement>) => {
    const button = event.currentTarget;
    const rect = button.getBoundingClientRect();
    button.style.setProperty("--button-x", `${((event.clientX - rect.left) / rect.width) * 100}%`);
    button.style.setProperty("--button-y", `${((event.clientY - rect.top) / rect.height) * 100}%`);
  }, []);

  const handleSubmitPointerLeave = useCallback((event: PointerEvent<HTMLButtonElement>) => {
    event.currentTarget.style.setProperty("--button-x", "50%");
    event.currentTarget.style.setProperty("--button-y", "50%");
  }, []);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    setSubmitting(true);
    try {
      const response = await apiClient.post<{
        data: { user: { username: string; role: "admin" | "viewer" } };
      }>("/auth/login", { username, password });
      setPassword("");
      onAuthenticated(response.data.data.user.username, response.data.data.user.role);
    } catch (cause) {
      setError(
        cause instanceof ApiClientError
          ? cause.message
          : "登录失败，请检查服务连接后重试",
      );
    } finally {
      setSubmitting(false);
    }
  }

  const panelStyle = {
    "--tilt-rx": `${tilt.rx}deg`,
    "--tilt-ry": `${tilt.ry}deg`,
    "--glare-x": `${tilt.mx}%`,
    "--glare-y": `${tilt.my}%`,
  } as CSSProperties;

  return (
    <main className="auth-page">
      <AuthAurora />
      <AuthParticleField />
      <div className="auth-page-inner">
        <section className="auth-intro" aria-labelledby="login-page-title">
          <div className="auth-brand">
            <span className="auth-mark" aria-hidden="true"><img src="/logo.svg" alt="" /></span>
            <span className="auth-brand-copy">
              <strong>All2API</strong>
              <small>Unified API gateway</small>
            </span>
          </div>
          <div className="auth-intro-copy">
            <p className="auth-kicker"><span aria-hidden="true" /> Gateway control plane</p>
            <h1 id="login-page-title">让每一次调用，<br /><span>更清晰。</span></h1>
            <p>统一管理渠道、模型与路由，把复杂的 API 流量收拢到一个可靠的控制台。</p>
          </div>
          <div className="auth-intro-features" aria-label="控制台能力">
            <span><i aria-hidden="true" /> Model routing</span>
            <span><i aria-hidden="true" /> Account pool</span>
            <span><i aria-hidden="true" /> Usage insight</span>
          </div>
        </section>

        <section
          className="auth-panel"
          ref={panelRef}
          style={panelStyle}
          aria-labelledby="login-title"
          onPointerMove={handlePanelPointerMove}
          onPointerLeave={handlePanelPointerLeave}
        >
          <span className="auth-panel-glare" aria-hidden="true" />
          <div className="auth-panel-inner">
            <div className="auth-panel-heading">
              <span className="auth-panel-eyebrow">Admin access</span>
              <h2 id="login-title">登录控制台</h2>
              <p>使用管理账户继续你的工作。</p>
            </div>
            <form className="auth-form" onSubmit={submit}>
              <label className="auth-field">
                <span className="auth-field-label">用户名</span>
                <span className="auth-input-wrap">
                  <UserIcon />
                  <input
                    autoComplete="username"
                    autoFocus
                    name="username"
                    placeholder="请输入用户名"
                    required
                    value={username}
                    onChange={(event) => setUsername(event.target.value)}
                  />
                </span>
              </label>
              <label className="auth-field">
                <span className="auth-field-label">密码</span>
                <span className="auth-input-wrap auth-password-control">
                  <LockIcon />
                  <input
                    autoComplete="current-password"
                    name="password"
                    placeholder="请输入密码"
                    required
                    type={showPassword ? "text" : "password"}
                    value={password}
                    onKeyDown={trackCapsLock}
                    onKeyUp={trackCapsLock}
                    onBlur={() => setCapsLock(false)}
                    onChange={(event) => setPassword(event.target.value)}
                  />
                  <button
                    className="auth-password-toggle"
                    type="button"
                    aria-label={showPassword ? "隐藏密码" : "显示密码"}
                    aria-pressed={showPassword}
                    title={showPassword ? "隐藏密码" : "显示密码"}
                    tabIndex={0}
                    onClick={() => setShowPassword((value) => !value)}
                  >
                    <EyeToggle />
                  </button>
                  {capsLock && (
                    <span className="auth-caps-hint" role="status">
                      大写锁定已开启
                    </span>
                  )}
                </span>
              </label>
              {error && (
                <p className="auth-error" role="alert" key={error}>
                  <span aria-hidden="true">!</span>
                  {error}
                </p>
              )}
              <button
                className="auth-submit"
                disabled={submitting}
                type="submit"
                aria-busy={submitting}
                onPointerMove={handleSubmitPointerMove}
                onPointerLeave={handleSubmitPointerLeave}
              >
                <span className="auth-submit-shine" aria-hidden="true" />
                <span className="auth-submit-cursor-glow" aria-hidden="true" />
                <span className="auth-submit-label">
                  {submitting && <span className="auth-spinner" aria-hidden="true" />}
                  {submitting ? "正在登录…" : "登录"}
                </span>
                <span className="auth-submit-arrow" aria-hidden="true">
                  <ArrowIcon />
                </span>
                <span className="auth-submit-progress" aria-hidden="true" />
              </button>
            </form>
            <div className="auth-panel-foot">
              <span className="auth-foot-icon" aria-hidden="true">
                <ShieldIcon />
              </span>
              <span>Protected admin session</span>
            </div>
          </div>
        </section>
      </div>
    </main>
  );
}
