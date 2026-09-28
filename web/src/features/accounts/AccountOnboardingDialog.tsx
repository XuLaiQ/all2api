import { useEffect, useMemo, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { Upload } from "lucide-react";
import { ApiClientError } from "../../api/client";
import type { ChangeEvent } from "react";
import type { ChannelOverview } from "../usage/usageApi";
import { Select } from "../../app/controls/Select";
import {
  cancelProvision,
  completeProvision,
  fetchProvisionSchema,
  importProvision,
  pollProvision,
  startProvision,
  type AccountOnboardingSession,
  type ProvisionFieldSchema,
  type ProvisionFlowSpec,
  type ProvisionSchema,
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

const terminalStatuses = new Set(["success", "succeeded", "expired", "invalid", "failed", "cancelled"]);

function errorMessage(cause: unknown): string {
  return cause instanceof ApiClientError ? cause.message : cause instanceof Error ? cause.message : "账号授权失败";
}

function configuredChannels(channels: ChannelOverview[]): ChannelOverview[] {
  return channels.filter((channel) => channel.provision_configured ?? channel.accounts_configured);
}

function channelStatusText(channel: ChannelOverview): string {
  if (!(channel.provision_configured ?? channel.accounts_configured)) return "未配置账号管理能力";
  if (!channel.enabled) return "账号能力已配置，渠道当前未启用模型服务";
  return "账号能力已配置";
}

function sessionStatusText(status: string | undefined): string {
  switch (status) {
    case "success":
    case "succeeded": return "授权成功";
    case "expired": return "授权已过期";
    case "invalid": return "授权已失效";
    case "failed": return "授权失败";
    case "cancelled": return "已取消";
    case "pending":
    case "waiting_user": return "等待授权";
    case "validating": return "正在校验";
    case "storing": return "正在保存";
    default: return status || "等待授权";
  }
}

function newIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return `acc_${crypto.randomUUID()}`;
  }
  return `acc_${Date.now()}_${Math.random().toString(36).slice(2)}`;
}

function defaultPayload(flow: ProvisionFlowSpec | undefined): Record<string, unknown> {
  const values: Record<string, unknown> = {};
  for (const [name, field] of Object.entries(flow?.schema.properties ?? {})) {
    if (field.default !== undefined) values[name] = field.default;
    else if (field.type === "boolean") values[name] = false;
    else if (field.type === "array") values[name] = [];
    else values[name] = "";
  }
  return values;
}

function fieldLabel(name: string, field: ProvisionFieldSchema): string {
  return field.title || name.replace(/[_-]+/g, " ").replace(/\b\w/g, (letter) => letter.toUpperCase());
}

function isMultiline(field: ProvisionFieldSchema): boolean {
  return field.format === "textarea" || field.format === "json" || field.type === "object" || field.type === "array";
}

function parseFieldValue(value: string, field: ProvisionFieldSchema): unknown {
  if (field.type === "number" || field.type === "integer") {
    if (value.trim() === "") return "";
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : value;
  }
  if (field.type === "boolean") return value === "true";
  if (field.type === "object" || field.type === "array" || field.format === "json") {
    if (!value.trim()) return field.type === "array" ? [] : {};
    try { return JSON.parse(value); } catch { return value; }
  }
  return value;
}

function stringifyFieldValue(value: unknown, field: ProvisionFieldSchema): string {
  if (value === undefined || value === null) return "";
  if (field.type === "object" || field.type === "array" || field.format === "json") {
    return typeof value === "string" ? value : JSON.stringify(value, null, 2);
  }
  return String(value);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function extractChatGptExportAccounts(value: unknown): Record<string, unknown>[] {
  const candidates = isRecord(value) && Array.isArray(value.accounts)
    ? value.accounts
    : Array.isArray(value)
      ? value
      : null;
  if (!candidates || candidates.length === 0 || !candidates.every(isRecord)) {
    throw new Error("JSON 文件中未找到有效的 accounts 数组");
  }
  return candidates;
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
  const unavailable = useMemo(
    () => channels.filter((item) => !(item.provision_configured ?? item.accounts_configured)),
    [channels],
  );
  const [channel, setChannel] = useState("");
  const [schema, setSchema] = useState<ProvisionSchema | null>(null);
  const [schemaLoading, setSchemaLoading] = useState(false);
  const [schemaError, setSchemaError] = useState("");
  const [flowId, setFlowId] = useState("");
  const [payload, setPayload] = useState<Record<string, unknown>>({});
  const [callback, setCallback] = useState("");
  const [session, setSession] = useState<AccountOnboardingSession | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [importFileName, setImportFileName] = useState("");
  const [importSummary, setImportSummary] = useState("");

  const selectedChannel = channels.find((item) => item.slug === channel);
  const selectedFlow = schema?.flows.find((item) => item.id === flowId);
  // Some channels expose a small setup flow before the actual login flow.
  // Doubao's profile setup is one example: once the profile has been created,
  // the dialog starts qr-login automatically and the returned session owns the
  // rest of the interaction.  Keep the user's selected flow intact so the
  // generic form reset effect does not discard the QR session.
  const activeFlow = session?.flow
    ? schema?.flows.find((item) => item.id === session.flow) ?? selectedFlow
    : selectedFlow;
  const canStart = Boolean(
    (selectedChannel?.provision_configured ?? selectedChannel?.accounts_configured)
      && selectedFlow
      && !schemaLoading,
  );

  useEffect(() => {
    if (open) return;
    setChannel("");
    setSchema(null);
    setSchemaError("");
    setFlowId("");
    setPayload({});
    setCallback("");
    setSession(null);
    setBusy(false);
    setMessage("");
    setError("");
    setImportFileName("");
    setImportSummary("");
  }, [open]);

  useEffect(() => {
    if (!open || !channel || !(selectedChannel?.provision_configured ?? selectedChannel?.accounts_configured)) return undefined;
    const controller = new AbortController();
    setSchema(null);
    setFlowId("");
    setPayload({});
    setCallback("");
    setSession(null);
    setSchemaLoading(true);
    setSchemaError("");
    setError("");
    setImportFileName("");
    setImportSummary("");
    fetchProvisionSchema(channel, controller.signal)
      .then((next) => setSchema(next))
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) setSchemaError(errorMessage(cause));
      })
      .finally(() => {
        if (!controller.signal.aborted) setSchemaLoading(false);
      });
    return () => controller.abort();
  }, [open, channel, selectedChannel?.provision_configured, selectedChannel?.accounts_configured]);

  useEffect(() => {
    setPayload(defaultPayload(selectedFlow));
    setCallback("");
    setSession(null);
    setMessage("");
    setError("");
    setImportFileName("");
    setImportSummary("");
  }, [selectedFlow]);

  useEffect(() => {
    if (!open || !channel || !session?.session_id || !activeFlow?.supports?.poll || terminalStatuses.has(session.status ?? "")) return undefined;
    let active = true;
    let timer: number | undefined;
    const poll = () => {
      pollProvision(channel, session.session_id as string)
        .then((next) => {
          if (!active) return;
          setSession((current) => ({ ...current, ...next }));
          if (next.message) setMessage(next.message);
          if (next.status === "success" || next.status === "succeeded") {
            onComplete(`${selectedChannel?.name ?? channel} 账号授权成功，账号列表已更新`);
          }
          if (next.status && terminalStatuses.has(next.status) && next.status !== "success" && next.status !== "succeeded") {
            setError(next.error || next.message || "账号授权失败");
          }
        })
        .catch((cause: unknown) => { if (active) setError(errorMessage(cause)); })
        .finally(() => {
          if (active) timer = window.setTimeout(poll, Math.max(2, session.poll_after_seconds ?? 2) * 1000);
        });
    };
    timer = window.setTimeout(poll, Math.max(2, session.poll_after_seconds ?? 2) * 1000);
    return () => {
      active = false;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [open, channel, session, activeFlow, selectedChannel?.name, onComplete]);

  if (!open) return null;

  function setField(name: string, field: ProvisionFieldSchema, value: string) {
    const parsed = parseFieldValue(value, field);
    if (name === "accounts" && isRecord(parsed) && Array.isArray(parsed.accounts)) {
      try {
        const accounts = extractChatGptExportAccounts(parsed);
        setPayload((current) => ({ ...current, accounts }));
        setImportFileName("");
        setImportSummary(`已解析 JSON 导出包：${accounts.length} 个账号`);
        setError("");
        return;
      } catch (cause: unknown) {
        setError(cause instanceof Error ? cause.message : "JSON 导出包格式不正确");
      }
    }
    setPayload((current) => ({ ...current, [name]: parsed }));
  }

  async function importChatGptExport(event: ChangeEvent<HTMLInputElement>) {
    const input = event.currentTarget;
    const file = input.files?.[0];
    if (!file) return;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      if (file.size > 10 * 1024 * 1024) throw new Error("JSON 文件不能超过 10 MB");
      const parsed: unknown = JSON.parse(await file.text());
      const accounts = extractChatGptExportAccounts(parsed);
      setPayload((current) => ({ ...current, accounts }));
      setImportFileName(file.name);
      setImportSummary(`已读取 ${accounts.length} 个账号，点击“开始授权”后批量导入`);
    } catch (cause: unknown) {
      setImportFileName("");
      setImportSummary("");
      setError(cause instanceof SyntaxError
        ? "JSON 文件无法解析，请确认文件内容完整"
        : cause instanceof Error ? cause.message : "JSON 导出包格式不正确");
    } finally {
      setBusy(false);
      input.value = "";
    }
  }

  function missingRequiredFields(): string[] {
    return (selectedFlow?.schema.required ?? []).filter((name) => {
      const value = payload[name];
      return value === undefined || value === null || (typeof value === "string" && !value.trim());
    });
  }

  async function startFlow() {
    if (!channel || !selectedFlow || !(selectedChannel?.provision_configured ?? selectedChannel?.accounts_configured)) {
      setError("请先选择已配置账号能力的渠道和新增方式");
      return;
    }
    const missing = missingRequiredFields();
    if (missing.length > 0) {
      setError(`请填写必填字段：${missing.join("、")}`);
      return;
    }
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const envelope = { flow: selectedFlow.id, payload, idempotency_key: newIdempotencyKey() };
      const importFlow = selectedFlow.supports?.import || selectedFlow.kind === "token_import" || selectedFlow.kind === "import";
      const result = importFlow
        ? await importProvision(channel, envelope)
        : await startProvision(channel, envelope);

      // Doubao's account is not usable until its browser profile has been
      // authenticated.  Chain the two native operations here so choosing the
      // normal "create profile" flow immediately produces a QR challenge in
      // this dialog instead of closing after creating an empty profile.
      if (
        channel === "doubao"
        && selectedFlow.id === "create-profile"
        && (result.status === "success" || result.status === "succeeded")
      ) {
        const accountId = String(payload.account_id ?? "").trim();
        const qrResult = await startProvision(channel, {
          flow: "qr-login",
          payload: { account_id: accountId },
          idempotency_key: newIdempotencyKey(),
        });
        setPayload({ account_id: accountId });
        setSession(qrResult);
        setMessage(qrResult.message || "账号 profile 已创建，请使用二维码完成登录");
        return;
      }

      setSession(result);
      setMessage(result.message || "流程已开始，请按页面提示继续操作");
      if (result.status === "success" || result.status === "succeeded") {
        onComplete(`${selectedChannel.name} 账号新增成功，账号列表已更新`);
      }
    } catch (cause: unknown) {
      setError(errorMessage(cause));
    } finally {
      setBusy(false);
    }
  }

  async function finishFlow() {
    if (!session?.session_id || !channel || !activeFlow) return;
    setBusy(true);
    setError("");
    try {
      const result = await completeProvision(channel, session.session_id, {
        payload: callback.trim() ? { ...payload, callback: callback.trim() } : payload,
        idempotency_key: newIdempotencyKey(),
      });
      setSession((current) => ({ ...current, ...result }));
      if (result.status === "success" || result.status === "succeeded") {
        onComplete(`${selectedChannel?.name ?? channel} 账号新增成功，账号列表已更新`);
      } else if (result.message) setMessage(result.message);
    } catch (cause: unknown) {
      setError(errorMessage(cause));
    } finally {
      setBusy(false);
    }
  }

  async function cancelFlow() {
    if (!session?.session_id || !channel || !activeFlow?.supports?.cancel) return;
    setBusy(true);
    setError("");
    try {
      const result = await cancelProvision(channel, session.session_id, newIdempotencyKey());
      setSession((current) => ({ ...current, ...result }));
      setMessage(result.message || "账号新增流程已取消");
    } catch (cause: unknown) {
      setError(errorMessage(cause));
    } finally {
      setBusy(false);
    }
  }

  const authUrl = session?.auth_url || session?.authorize_url || "";
  const qrCode = session?.qr_code?.trim() || "";
  const qrImageValue = session?.qr_image_base64?.trim() || "";
  // Browser workers can return either raw base64, a data URI, or (for a
  // remote worker) an image URL. Normalize all three forms before rendering.
  const qrImage = qrImageValue
    ? (/^(?:data:image\/|https?:\/\/)/i.test(qrImageValue)
      ? qrImageValue
      : `data:image/png;base64,${qrImageValue.replace(/\s+/g, "")}`)
    : /^data:image\//i.test(qrCode)
      ? qrCode
      : "";
  const qrValue = qrImage ? "" : authUrl || qrCode;
  const flowNeedsCompletion = Boolean(session && activeFlow?.supports?.complete);
  const isChatGptTokenImport = channel === "chatgpt" && selectedFlow?.id === "token-import";

  return (
    <div className="key-modal-backdrop account-onboarding-backdrop" role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget && !busy) onClose();
    }}>
      <section className="key-editor account-onboarding-dialog" role="dialog" aria-modal="true" aria-labelledby="account-onboarding-title">
        <div className="section-heading">
          <div><h2 id="account-onboarding-title">新增账号</h2><p>先选择渠道和新增方式，表单字段由该渠道能力声明动态提供。</p></div>
          <button type="button" className="icon-close" aria-label="关闭" onClick={onClose} disabled={busy}>×</button>
        </div>

        {error && <div className="notice notice-error" role="alert">{error}</div>}
        {message && <div className="notice notice-info" role="status">{message}</div>}

        <div className="onboarding-form">
          <label><span>渠道</span><Select
            value={channel}
            onChange={setChannel}
            disabled={busy || channelsLoading || Boolean(session)}
            placeholder={channelsLoading ? "正在读取渠道配置…" : "请选择渠道"}
            options={[
              { group: "可新增账号", options: available.map((item) => ({ value: item.slug, label: item.name })) },
              {
                group: "需要先配置账号能力",
                options: unavailable.map((item) => ({ value: item.slug, label: `${item.name}（未配置）`, disabled: true })),
              },
            ]}
          /></label>

          {channelsLoading && <div className="onboarding-channel-status" role="status">正在读取渠道账号能力…</div>}
          {!channelsLoading && channelsError && <div className="notice notice-error onboarding-channel-notice" role="alert"><span>{channelsError}</span>{onRetryChannels && <button type="button" onClick={onRetryChannels} disabled={busy}>重试</button>}</div>}
          {!channelsLoading && !channelsError && channels.length === 0 && <div className="notice notice-warning onboarding-channel-notice" role="status">暂未读取到已注册渠道。</div>}
          {!channelsLoading && !channelsError && channels.length > 0 && available.length === 0 && <div className="notice notice-warning onboarding-channel-notice" role="status">当前没有已配置账号能力的渠道。</div>}
          {!channelsLoading && !channelsError && selectedChannel && <div className={`onboarding-channel-status ${(selectedChannel.provision_configured ?? selectedChannel.accounts_configured) ? "is-ready" : "is-unavailable"}`} role="status"><span className={`status-pill ${(selectedChannel.provision_configured ?? selectedChannel.accounts_configured) ? "success" : "warning"}`}><i />{channelStatusText(selectedChannel)}</span></div>}

          {channel && schemaLoading && <div className="table-state" role="status">正在读取该渠道的新增方式…</div>}
          {channel && !schemaLoading && schemaError && <div className="notice notice-error" role="alert">读取渠道新增能力失败：{schemaError}</div>}
          {channel && !schemaLoading && !schemaError && schema && schema.flows.length === 0 && <div className="notice notice-warning" role="status">该渠道当前没有可用的账号新增方式。</div>}
          {schema && schema.flows.length > 0 && <label><span>新增方式</span><Select value={flowId} onChange={setFlowId} disabled={busy || Boolean(session)} placeholder="请选择新增方式" options={schema.flows.map((flow) => ({ value: flow.id, label: flow.title || flow.id }))} /></label>}
          {activeFlow?.description && <p className="secondary-text">{activeFlow.description}</p>}

          {isChatGptTokenImport && !session && <div className="onboarding-import-tools">
            <div className="onboarding-import-heading">
              <div><strong>导入 ChatGPT JSON</strong><small>支持 sub2api-export 导出文件，凭据只会提交到服务端加密存储。</small></div>
              <Upload size={17} strokeWidth={1.8} aria-hidden="true" />
            </div>
            <label className="onboarding-file-picker">
              <span>选择 JSON 文件</span>
              <input type="file" accept=".json,application/json" onChange={(event) => void importChatGptExport(event)} disabled={busy} />
            </label>
            {(importFileName || importSummary) && <p className="onboarding-import-summary"><strong>{importFileName || "已粘贴 JSON"}</strong>{importSummary}</p>}
          </div>}

          {activeFlow && Object.entries(activeFlow.schema.properties ?? {}).map(([name, field]) => {
            const value = payload[name];
            const label = fieldLabel(name, field);
            const required = activeFlow.schema.required?.includes(name) ?? false;
            if (isChatGptTokenImport && name === "accounts" && importSummary) {
              return <div key={name} className="onboarding-import-loaded" role="status">
                <div><strong>{label}</strong><span>{importSummary}</span></div>
                <button type="button" className="secondary-action" onClick={() => { setPayload((current) => ({ ...current, accounts: [] })); setImportFileName(""); setImportSummary(""); }} disabled={busy}>清除</button>
              </div>;
            }
            if (field.enum?.length) return <label key={name}><span>{label}{required ? " *" : ""}</span><Select value={String(value ?? "")} onChange={(selected) => setField(name, field, selected)} disabled={busy || Boolean(session)} placeholder="请选择" options={field.enum.map((option) => ({ value: String(option), label: String(option) }))} />{field.description && <small>{field.description}</small>}</label>;
            if (field.type === "boolean") return <label key={name} className="checkbox-label"><input type="checkbox" checked={Boolean(value)} onChange={(event) => setPayload((current) => ({ ...current, [name]: event.target.checked }))} disabled={busy || Boolean(session)} /><span>{label}{required ? " *" : ""}</span>{field.description && <small>{field.description}</small>}</label>;
            return <label key={name}><span>{label}{required ? " *" : ""}</span>{isMultiline(field) ? <textarea value={stringifyFieldValue(value, field)} onChange={(event) => setField(name, field, event.target.value)} rows={4} disabled={busy || Boolean(session)} /> : <input type={field.secret || field.format === "password" ? "password" : field.type === "number" || field.type === "integer" ? "number" : "text"} value={stringifyFieldValue(value, field)} onChange={(event) => setField(name, field, event.target.value)} minLength={field.minLength} maxLength={field.maxLength} disabled={busy || Boolean(session)} />}{field.description && <small>{field.description}</small>}</label>;
          })}

          {session && (qrValue || qrImage) && <div className="onboarding-authorize-box">
            {authUrl && <a href={authUrl} target="_blank" rel="noreferrer" className="onboarding-url">{authUrl}</a>}
            {qrImage ? <img className="onboarding-qr-image" src={qrImage} alt="账号授权二维码" /> : <QRCodeSVG value={qrValue} size={210} level="M" />}
            <span className="onboarding-session-status">{sessionStatusText(session.status)}</span>
          </div>}
          {session && !qrValue && !qrImage && <div className="onboarding-authorize-box"><span className="onboarding-session-status">{sessionStatusText(session.status)}</span></div>}
          {flowNeedsCompletion && session && <label><span>授权回调（如有）</span><textarea value={callback} onChange={(event) => setCallback(event.target.value)} rows={3} placeholder="按渠道提示粘贴回调 URL 或 code" disabled={busy} /></label>}
        </div>

        <div className="log-filter-actions onboarding-actions">
          {!session && <button type="button" onClick={() => void startFlow()} disabled={busy || !canStart}>{busy ? "准备中…" : "开始授权"}</button>}
          {flowNeedsCompletion && session && <button type="button" onClick={() => void finishFlow()} disabled={busy}>{busy ? "提交中…" : "完成授权"}</button>}
          {session && activeFlow?.supports?.cancel && !terminalStatuses.has(session.status ?? "") && <button type="button" className="secondary-action" onClick={() => void cancelFlow()} disabled={busy}>取消流程</button>}
          {session && (error || terminalStatuses.has(session.status ?? "")) && <button type="button" className="secondary-action" onClick={() => { setSession(null); setMessage(""); setError(""); }} disabled={busy}>重新开始</button>}
          <button type="button" className="secondary-action" onClick={onClose} disabled={busy}>关闭</button>
        </div>
        {unavailable.length > 0 && <p className="onboarding-channel-footnote">标记为“未配置”的渠道需要先在服务端补齐账号能力配置。</p>}
      </section>
    </div>
  );
}
