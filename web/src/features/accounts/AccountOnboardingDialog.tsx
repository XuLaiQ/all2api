import { useEffect, useMemo, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { ApiClientError } from "../../api/client";
import type { ChannelOverview } from "../usage/usageApi";
import {
  finishAccountOnboarding,
  pollAccountOnboarding,
  startAccountOnboarding,
  type AccountOnboardingSession,
} from "./accountsApi";

type Props = {
  open: boolean;
  channels: ChannelOverview[];
  channelsLoading?: boolean;
  channelsError?: string;
  onRetryChannels?: () => void;
  onClose: () => void;
  onComplete: (message: string) => void;
};

type Mode = "oauth" | "token";

const globalRegions = ["HK", "MO", "SG", "TH", "PH", "MY", "ID"];

function errorMessage(cause: unknown): string {
  return cause instanceof ApiClientError ? cause.message : cause instanceof Error ? cause.message : "账号授权失败";
}

function configuredChannels(channels: ChannelOverview[]): ChannelOverview[] {
  return channels.filter((channel) => channel.accounts_configured);
}

function channelStatusText(channel: ChannelOverview): string {
  if (!channel.accounts_configured) return "未配置账号管理接口";
  if (!channel.enabled) return "账号接口已配置，可新增（渠道未启用模型服务）";
  return "账号接口已配置，可新增";
}

function sessionStatusText(status: string | undefined): string {
  switch (status) {
    case "success": return "授权成功";
    case "expired": return "授权已过期";
    case "invalid": return "授权已失效";
    case "failed": return "授权失败";
    case "pending": return "等待授权";
    default: return status || "等待授权";
  }
}

export function AccountOnboardingDialog({
  open,
  channels,
  channelsLoading = false,
  channelsError = "",
  onRetryChannels,
  onClose,
  onComplete,
}: Props) {
  const available = useMemo(() => configuredChannels(channels), [channels]);
  const unavailable = useMemo(() => channels.filter((item) => !item.accounts_configured), [channels]);
  const [channel, setChannel] = useState("");
  const [realm, setRealm] = useState<"cn" | "global">("cn");
  const [region, setRegion] = useState("");
  const [mode, setMode] = useState<Mode>("oauth");
  const [accountId, setAccountId] = useState("");
  const [accountName, setAccountName] = useState("");
  const [emailHint, setEmailHint] = useState("");
  const [token, setToken] = useState("");
  const [callback, setCallback] = useState("");
  const [session, setSession] = useState<AccountOnboardingSession | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    if (!open) return;
    const initial = available[0]?.slug ?? "";
    setChannel(initial);
    setRealm("cn");
    setRegion("");
    setMode("oauth");
    setAccountId("");
    setAccountName("");
    setEmailHint("");
    setToken("");
    setCallback("");
    setSession(null);
    setBusy(false);
    setMessage("");
    setError("");
  }, [open, available]);

  useEffect(() => {
    if (!open || !session || session.flow !== "qr") return undefined;
    if (session.status === "success" || session.status === "expired" || session.status === "invalid" || session.status === "failed") return undefined;
    let active = true;
    const timer = window.setInterval(() => {
      if (busy) return;
      const params = session.channel === "wb"
        ? { state: session.state, realm, region: region || undefined }
        : { account_id: session.account_id };
      pollAccountOnboarding(session.channel, params)
        .then((next) => {
          if (!active) return;
          setSession((current) => ({ ...current, ...next }));
          if (next.message) setMessage(next.message);
          if (next.status === "success") {
            onComplete(`${session.channel === "doubao" ? "Doubao" : "WorkBuddy"} 账号授权成功，正在同步账号`);
          }
          if (["expired", "invalid", "failed"].includes(next.status ?? "")) {
            setError(next.error || next.message || "二维码授权失败");
          }
        })
        .catch((cause: unknown) => {
          if (active) setError(errorMessage(cause));
        });
    }, 2000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [open, session, realm, region, busy, onComplete]);

  if (!open) return null;

  const selectedChannel = channels.find((item) => item.slug === channel);
  const canStart = Boolean(selectedChannel?.accounts_configured);
  const isWorkBuddy = channel === "wb";
  const isDoubao = channel === "doubao";
  const isChatGPT = channel === "chatgpt";
  const qrImage = session?.qr_image_base64 ? `data:image/png;base64,${session.qr_image_base64}` : "";
  const authUrl = session?.auth_url || session?.authorize_url || "";

  function resetAuthorization() {
    setSession(null);
    setCallback("");
    setMessage("");
    setError("");
  }

  async function startFlow() {
    if (!selectedChannel || !selectedChannel.accounts_configured) {
      setError("请选择一个已配置账号管理接口的渠道");
      return;
    }
    setBusy(true);
    setError("");
    setMessage("");
    try {
      if (isChatGPT && mode === "token") {
        if (!token.trim()) throw new Error("请先粘贴 Access Token");
        const result = await finishAccountOnboarding("chatgpt", { tokens: [token.trim()] });
        onComplete(`ChatGPT 已新增 ${result.added ?? 0} 个账号，正在同步账号`);
        return;
      }
      const result = await startAccountOnboarding(channel, {
        realm,
        account_id: accountId.trim(),
        name: accountName.trim(),
        email_hint: emailHint.trim(),
      });
      setSession(result);
      setMessage(result.message || (result.flow === "oauth" ? "请打开授权链接完成登录" : "请完成扫码授权"));
    } catch (cause: unknown) {
      setError(errorMessage(cause));
    } finally {
      setBusy(false);
    }
  }

  async function finishOAuth() {
    if (!session?.session_id || !callback.trim()) return;
    setBusy(true);
    setError("");
    try {
      const result = await finishAccountOnboarding("chatgpt", {
        session_id: session.session_id,
        callback: callback.trim(),
      });
      onComplete(`ChatGPT 已新增 ${result.added ?? 0} 个账号，正在同步账号`);
    } catch (cause: unknown) {
      setError(errorMessage(cause));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="key-modal-backdrop account-onboarding-backdrop" role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget && !busy) onClose();
    }}>
      <section className="key-editor account-onboarding-dialog" role="dialog" aria-modal="true" aria-labelledby="account-onboarding-title">
        <div className="section-heading">
          <div><h2 id="account-onboarding-title">新增账号</h2><p>调用对应渠道原生的授权流程，凭据不会写入 All2API。</p></div>
          <button type="button" className="icon-close" aria-label="关闭" onClick={onClose} disabled={busy}>×</button>
        </div>

        {error && <div className="notice notice-error" role="alert">{error}</div>}
        {message && <div className="notice notice-info" role="status">{message}</div>}

        <div className="onboarding-form">
          <label><span>渠道</span><select value={channel} onChange={(event) => { setChannel(event.target.value); setSession(null); setMessage(""); setError(""); }} disabled={busy || channelsLoading || Boolean(session)}>
            <option value="" disabled>{channelsLoading ? "正在读取渠道配置…" : "请选择可新增账号的渠道"}</option>
            {available.length > 0 && <optgroup label="可新增账号">
              {available.map((item) => <option key={item.slug} value={item.slug}>{item.name}</option>)}
            </optgroup>}
            {unavailable.length > 0 && <optgroup label="需要先配置账号接口">
              {unavailable.map((item) => <option key={item.slug} value={item.slug} disabled>{item.name}（未配置）</option>)}
            </optgroup>}
          </select></label>

          {channelsLoading && <div className="onboarding-channel-status" role="status">正在读取渠道的账号管理能力…</div>}
          {!channelsLoading && channelsError && <div className="notice notice-error onboarding-channel-notice" role="alert">
            <span>{channelsError}，暂时无法判断哪些渠道可以新增账号。</span>
            {onRetryChannels && <button type="button" onClick={onRetryChannels} disabled={busy}>重试</button>}
          </div>}
          {!channelsLoading && !channelsError && channels.length === 0 && <div className="notice notice-warning onboarding-channel-notice" role="status">
            <span>暂未读取到已注册渠道，请检查服务端渠道配置后重试。</span>
            {onRetryChannels && <button type="button" onClick={onRetryChannels} disabled={busy}>重新读取</button>}
          </div>}
          {!channelsLoading && !channelsError && channels.length > 0 && available.length === 0 && <div className="notice notice-warning onboarding-channel-notice" role="status">
            <span>当前没有可直接新增账号的渠道。下方渠道都缺少账号管理接口配置，请先补齐上游地址和管理凭据。</span>
          </div>}
          {!channelsLoading && !channelsError && selectedChannel && <div className={`onboarding-channel-status ${canStart ? "is-ready" : "is-unavailable"}`} role="status">
            <span className={`status-pill ${canStart ? "success" : "warning"}`}><i />{channelStatusText(selectedChannel)}</span>
            <small>{canStart
              ? "凭据只会交给对应上游服务处理，不会写入本地账号快照。"
              : `缺少配置：${selectedChannel.account_config?.missing_env?.join("、") || "账号管理凭据"}。补齐后重新读取渠道。`}</small>
          </div>}

          {isWorkBuddy && <>
            <label><span>账号版本</span><select value={realm} onChange={(event) => { setRealm(event.target.value as "cn" | "global"); setRegion(""); }} disabled={busy || Boolean(session)}><option value="cn">国内版</option><option value="global">国际版</option></select></label>
            {realm === "global" && <label><span>地区</span><select value={region} onChange={(event) => setRegion(event.target.value)} disabled={busy || Boolean(session)}><option value="">选择地区</option>{globalRegions.map((item) => <option key={item} value={item}>{item}</option>)}</select></label>}
          </>}
          {isDoubao && <div className="onboarding-two-columns"><label><span>账号 ID</span><input value={accountId} onChange={(event) => setAccountId(event.target.value)} placeholder="留空自动生成" disabled={busy || Boolean(session)} /></label><label><span>显示名称</span><input value={accountName} onChange={(event) => setAccountName(event.target.value)} placeholder="可选" disabled={busy || Boolean(session)} /></label></div>}
          {isChatGPT && <>
            <div className="segmented-tabs onboarding-mode-tabs" role="tablist" aria-label="ChatGPT 新增方式"><button type="button" className={mode === "oauth" ? "active" : ""} onClick={() => setMode("oauth")} disabled={busy || Boolean(session)}>OAuth 登录</button><button type="button" className={mode === "token" ? "active" : ""} onClick={() => setMode("token")} disabled={busy || Boolean(session)}>Token 导入</button></div>
            {mode === "oauth" ? <label><span>邮箱提示（可选）</span><input value={emailHint} onChange={(event) => setEmailHint(event.target.value)} placeholder="用于预填登录邮箱" disabled={busy || Boolean(session)} /></label> : <label><span>Access Token</span><textarea value={token} onChange={(event) => setToken(event.target.value)} placeholder="粘贴 ChatGPT access token" rows={4} disabled={busy || Boolean(session)} /></label>}
          </>}

          {session?.flow === "qr" && <div className="onboarding-authorize-box">
            {qrImage ? <img className="onboarding-qr-image" src={qrImage} alt="账号授权二维码" /> : authUrl ? <QRCodeSVG value={authUrl} size={210} level="M" /> : <div className="table-state onboarding-waiting">正在准备授权…</div>}
            {authUrl && <a href={authUrl} target="_blank" rel="noreferrer" className="text-link onboarding-auth-link">打开授权链接</a>}
            <span className="onboarding-session-status">{sessionStatusText(session.status)}</span>
            <small>{isDoubao ? "请使用豆包 App 扫码，页面会自动轮询登录状态。" : "请完成扫码或浏览器授权，页面会自动轮询登录状态。"}</small>
          </div>}

          {session?.flow === "oauth" && <div className="onboarding-oauth-box">
            {authUrl && <a href={authUrl} target="_blank" rel="noreferrer" className="onboarding-url">{authUrl}</a>}
            <label><span>授权回调 URL / code</span><textarea value={callback} onChange={(event) => setCallback(event.target.value)} rows={3} placeholder="完成登录后粘贴回调 URL 或 code" disabled={busy} /></label>
          </div>}
        </div>

        <div className="log-filter-actions onboarding-actions">
          {!session && <button type="button" onClick={() => void startFlow()} disabled={busy || !canStart}>{busy ? "准备中…" : isChatGPT && mode === "token" ? "导入账号" : "开始授权"}</button>}
          {session?.flow === "oauth" && <button type="button" onClick={() => void finishOAuth()} disabled={busy || !callback.trim()}>{busy ? "提交中…" : "完成授权"}</button>}
          {session && (error || ["expired", "invalid", "failed"].includes(session.status ?? "")) && <button type="button" className="secondary-action" onClick={resetAuthorization} disabled={busy}>重新开始</button>}
          <button type="button" className="secondary-action" onClick={onClose} disabled={busy}>关闭</button>
        </div>
        {unavailable.length > 0 && <p className="onboarding-channel-footnote">标记为“未配置”的渠道只代表当前服务端没有可调用的账号管理接口；补齐配置后重新打开此窗口即可使用。</p>}
      </section>
    </div>
  );
}
