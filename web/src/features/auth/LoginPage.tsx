import { useState, type FormEvent } from "react";
import { ApiClientError, apiClient } from "../../api/client";

interface LoginPageProps {
  onAuthenticated: (username: string, role: "admin" | "viewer") => void;
  initialError?: string;
}

export function LoginPage({ onAuthenticated, initialError }: LoginPageProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState(initialError ?? "");
  const [submitting, setSubmitting] = useState(false);

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

  return (
    <main className="auth-page">
      <section className="auth-panel" aria-labelledby="login-title">
        <div className="auth-brand">
          <span className="auth-mark" aria-hidden="true"><img src="/logo.svg" alt="" /></span>
          <span>All2API</span>
        </div>
        <h1 id="login-title">登录管理控制台</h1>
        <form className="auth-form" onSubmit={submit}>
          <label>
            用户名
            <input
              autoComplete="username"
              autoFocus
              name="username"
              required
              value={username}
              onChange={(event) => setUsername(event.target.value)}
            />
          </label>
          <label>
            密码
            <input
              autoComplete="current-password"
              name="password"
              required
              type="password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
          </label>
          {error && <p className="auth-error" role="alert">{error}</p>}
          <button className="auth-submit" disabled={submitting} type="submit">
            {submitting ? "正在登录…" : "登录"}
          </button>
        </form>
      </section>
    </main>
  );
}
