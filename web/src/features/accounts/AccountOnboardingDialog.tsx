import { useEffect, useMemo, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import {
  ArrowLeft,
  FileJson,
  FileText,
  Files,
  KeyRound,
  LoaderCircle,
  LogIn,
  Upload,
} from "lucide-react";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
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
  onComplete: (message: string, channel?: string) => void;
};

type ChatGptImportMethod = "menu" | "oauth" | "token" | "session" | "codex" | "account-json";

type PendingChatGptImport = {
  accounts: Record<string, unknown>[];
  tokens: string[];
  fileName: string;
  errorCount: number;
};

const terminalStatuses = new Set(["success", "succeeded", "expired", "invalid", "failed", "cancelled"]);

const channelLabels: Record<string, string> = {
  doubao: "豆包",
  chatgpt: "ChatGPT",
  wb: "WorkBuddy",
};

const hiddenFlowIds: Record<string, Set<string>> = {
  // Profile creation is an internal step of QR login. Operators should only
  // choose the actual login/import methods exposed by the product.
  doubao: new Set(["create-profile"]),
};

const flowCopy: Record<string, { title: string; description: string }> = {
  "doubao:qr-login": {
    title: "二维码登录",
    description: "显示豆包登录二维码，使用手机扫码完成账号授权。",
  },
  "doubao:cookie-import": {
    title: "导入 Cookie",
    description: "粘贴从豆包浏览器开发者工具复制的 Cookie，直接导入已有账号。",
  },
  "wb:qr-oauth": {
    title: "二维码登录",
    description: "打开 WorkBuddy 登录二维码，扫码完成账号授权。",
  },
  "wb:qr-auth": {
    title: "二维码登录",
    description: "打开 WorkBuddy 登录二维码，扫码完成账号授权。",
  },
  "wb:token-import": {
    title: "导入账号凭据",
    description: "粘贴已有的 WorkBuddy 账号凭据 JSON。",
  },
  "chatgpt:oauth-pkce": {
    title: "网页登录",
    description: "打开 ChatGPT 登录页完成登录，再粘贴浏览器回调地址。",
  },
  "chatgpt:token-import": {
    title: "导入访问令牌",
    description: "导入已有的 ChatGPT 访问令牌或账号凭据。",
  },
};

const fieldLabels: Record<string, string> = {
  account_id: "账号标识",
  name: "账号名称",
  priority: "账号优先级",
  enabled: "启用账号",
  realm: "登录区域",
  region: "业务区域（可选）",
  email_hint: "邮箱提示（可选）",
  callback: "授权回调地址",
  cookie: "Cookie 内容",
  cookies: "Cookie 内容",
  accounts: "批量账号凭据",
  tokens: "访问令牌",
};

const fieldDescriptions: Record<string, string> = {
  account_id: "可选的内部标识；留空时由系统自动生成。",
  name: "用于在账号池中识别该账号，不影响登录。",
  priority: "账号选择优先级，数值越大越优先；留空使用默认值 0。",
  enabled: "关闭后账号不会参与请求转发，可在账号列表中重新启用。",
  realm: "选择账号所在的服务区域。",
  region: "可选的业务区域标识，通常留空即可。",
  email_hint: "可选。用于提示登录页面优先使用哪个邮箱，不会代替登录。",
  callback: "完成网页登录后，从浏览器地址栏复制完整的回调地址粘贴到这里。",
  cookie: "从豆包浏览器开发者工具复制完整 Cookie 请求头，内容必须包含 sessionid。",
  cookies: "可粘贴浏览器导出的 Cookie 列表；单个账号直接使用上面的 Cookie 内容。",
  accounts: "可批量导入多个账号凭据；不需要批量导入时留空即可。",
  tokens: "每行填写一个访问令牌。",
};

const enumLabels: Record<string, Record<string, string>> = {
  realm: {
    cn: "中国大陆",
    global: "国际区域",
  },
};

function errorMessage(cause: unknown): string {
  return cause instanceof ApiClientError ? cause.message : cause instanceof Error ? cause.message : "账号授权失败";
}

function configuredChannels(channels: ChannelOverview[]): ChannelOverview[] {
  return channels.filter((channel) => channel.provision_configured ?? channel.accounts_configured);
}

function channelLabel(slug: string, fallback = slug): string {
  return channelLabels[slug] ?? fallback;
}

function selectableFlows(channel: string, flows: ProvisionFlowSpec[]): ProvisionFlowSpec[] {
  const hidden = hiddenFlowIds[channel];
  return hidden ? flows.filter((flow) => !hidden.has(flow.id)) : flows;
}

function flowPresentation(channel: string, flow: ProvisionFlowSpec): { title: string; description: string } {
  const copy = flowCopy[`${channel}:${flow.id}`];
  return {
    title: copy?.title || flow.title || flow.id.replace(/[_-]+/g, " "),
    description: copy?.description || flow.description || "按页面提示完成账号新增。",
  };
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
  }
  return values;
}

function fieldLabel(name: string, field: ProvisionFieldSchema): string {
  return fieldLabels[name] || field.title || name.replace(/[_-]+/g, " ").replace(/\b\w/g, (letter) => letter.toUpperCase());
}

function fieldDescription(name: string, field: ProvisionFieldSchema): string {
  return fieldDescriptions[name] || field.description || "可选配置项。";
}

function enumOptionLabel(name: string, option: string | number | boolean): string {
  return enumLabels[name]?.[String(option)] || String(option);
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

function splitTokens(value: string): string[] {
  return value.split(/\r?\n/).map((item) => item.trim()).filter(Boolean);
}

function tokenFromRecord(value: Record<string, unknown>): string {
  const source = isRecord(value.credentials) ? value.credentials : value;
  const token = source.access_token ?? source.accessToken ?? source.token;
  return typeof token === "string" ? token.trim() : "";
}

function normalizeChatGptAccount(value: unknown): Record<string, unknown> | null {
  if (!isRecord(value)) return null;
  const token = tokenFromRecord(value);
  return token ? { ...value, access_token: token } : null;
}

function extractChatGptImportAccounts(value: unknown): Record<string, unknown>[] {
  if (Array.isArray(value)) {
    return value.map(normalizeChatGptAccount).filter((item): item is Record<string, unknown> => Boolean(item));
  }
  if (!isRecord(value)) return [];
  const nested = value.accounts ?? value.items;
  if (Array.isArray(nested)) {
    return nested.map(normalizeChatGptAccount).filter((item): item is Record<string, unknown> => Boolean(item));
  }
  const single = normalizeChatGptAccount(value);
  return single ? [single] : [];
}

function ChatGptMethodCard({
  title,
  description,
  icon: Icon,
  onClick,
}: {
  title: string;
  description: string;
  icon: typeof KeyRound;
  onClick: () => void;
}) {
  return (
    <Button variant="unstyled" className="onboarding-method-card" onClick={onClick}>
      <span className="onboarding-method-icon" aria-hidden="true"><Icon size={18} /></span>
      <span><strong>{title}</strong><small>{description}</small></span>
    </Button>
  );
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
  const [chatGptImportMethod, setChatGptImportMethod] = useState<ChatGptImportMethod>("menu");
  const [chatGptTokenInput, setChatGptTokenInput] = useState("");
  const [chatGptSessionInput, setChatGptSessionInput] = useState("");
  const [chatGptCodexInput, setChatGptCodexInput] = useState("");
  const [pendingChatGptImport, setPendingChatGptImport] = useState<PendingChatGptImport | null>(null);
  const [chatGptImportBusy, setChatGptImportBusy] = useState(false);

  const selectedChannel = channels.find((item) => item.slug === channel);
  const availableFlows = useMemo(
    () => selectableFlows(channel, schema?.flows ?? []),
    [channel, schema],
  );
  const selectedFlow = availableFlows.find((item) => item.id === flowId);
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
    setChatGptImportMethod("menu");
    setChatGptTokenInput("");
    setChatGptSessionInput("");
    setChatGptCodexInput("");
    setPendingChatGptImport(null);
    setChatGptImportBusy(false);
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
    setChatGptImportMethod("menu");
    setChatGptTokenInput("");
    setChatGptSessionInput("");
    setChatGptCodexInput("");
    setPendingChatGptImport(null);
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
    if (selectedFlow?.id !== "token-import") setPendingChatGptImport(null);
  }, [selectedFlow]);

  useEffect(() => {
    if (!open || !channel || !session?.session_id || !activeFlow?.supports?.poll || terminalStatuses.has(session.status ?? "")) return undefined;
    let active = true;
    let completing = false;
    let timer: number | undefined;
    const completionPayload = (): Record<string, unknown> => {
      const next: Record<string, unknown> = {};
      if (channel === "wb" && typeof payload.region === "string" && payload.region.trim()) {
        next.region = payload.region.trim();
      }
      if (callback.trim()) next.callback = callback.trim();
      return next;
    };
    const completeReadyWorkBuddySession = async (ready: AccountOnboardingSession) => {
      if (!ready.session_id || completing) return;
      completing = true;
      setBusy(true);
      setMessage("登录成功，正在保存账号…");
      try {
        const completed = await completeProvision(channel, ready.session_id, {
          payload: completionPayload(),
          idempotency_key: newIdempotencyKey(),
        });
        if (!active) return;
        setSession((current) => ({ ...current, ...completed }));
        if (completed.status === "success" || completed.status === "succeeded") {
          onComplete(`${selectedChannel?.name ?? channel} 账号授权成功，账号列表已更新`, channel);
        } else if (completed.error || completed.message) {
          setError(completed.error || completed.message || "账号保存失败");
        }
      } catch (cause: unknown) {
        if (active) setError(errorMessage(cause));
      } finally {
        if (active) setBusy(false);
      }
    };
    const poll = () => {
      pollProvision(channel, session.session_id as string)
        .then((next) => {
          if (!active) return;
          if (next.next_step === "complete") {
            void completeReadyWorkBuddySession(next);
            return;
          }
          setSession((current) => ({ ...current, ...next }));
          if (next.message) setMessage(next.message);
          if (next.status === "success" || next.status === "succeeded") {
            onComplete(`${selectedChannel?.name ?? channel} 账号授权成功，账号列表已更新`, channel);
          }
          if (next.status && terminalStatuses.has(next.status) && next.status !== "success" && next.status !== "succeeded") {
            setError(next.error || next.message || "账号授权失败");
          }
        })
        .catch((cause: unknown) => { if (active) setError(errorMessage(cause)); })
        .finally(() => {
          if (active && !completing) timer = window.setTimeout(poll, Math.max(2, session.poll_after_seconds ?? 2) * 1000);
        });
    };
    timer = window.setTimeout(poll, Math.max(2, session.poll_after_seconds ?? 2) * 1000);
    return () => {
      active = false;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [open, channel, session, activeFlow, payload, callback, selectedChannel?.name, onComplete]);

  if (!open) return null;

  function setField(name: string, field: ProvisionFieldSchema, value: string) {
    const parsed = parseFieldValue(value, field);
    setPayload((current) => ({ ...current, [name]: parsed }));
  }

  function missingRequiredFields(sourcePayload = payload): string[] {
    return (selectedFlow?.schema.required ?? []).filter((name) => {
      if (name === "account_id") return false;
      const value = sourcePayload[name];
      return value === undefined || value === null || (typeof value === "string" && !value.trim());
    });
  }

  async function startFlow(payloadOverride?: Record<string, unknown>) {
    if (!channel || !selectedFlow || !(selectedChannel?.provision_configured ?? selectedChannel?.accounts_configured)) {
      setError("请先选择已配置账号能力的渠道和新增方式");
      return;
    }
    const flowPayload = payloadOverride ?? payload;
    const missing = missingRequiredFields(flowPayload);
    if (missing.length > 0) {
      setError(`请填写必填字段：${missing.map((name) => fieldLabel(name, selectedFlow.schema.properties?.[name] ?? {})).join("、")}`);
      return;
    }
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const envelope = { flow: selectedFlow.id, payload: flowPayload, idempotency_key: newIdempotencyKey() };
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
        const account = isRecord(result.account) ? result.account : {};
        const accountId = String(
          account.native_id
            ?? account.account_id
            ?? String(account.id ?? "").replace(/^doubao:/, "")
            ?? "",
        ).trim();
        if (!accountId) throw new Error("豆包账号 profile 创建成功，但未返回内部账号标识");
        const qrResult = await startProvision(channel, {
          flow: "qr-login",
          payload: { account_id: accountId },
          idempotency_key: newIdempotencyKey(),
        });
        setPayload({});
        setSession(qrResult);
        setMessage(qrResult.message || "账号 profile 已创建，请使用二维码完成登录");
        return;
      }

      setSession(result);
      const importSummary = importFlow && (
        typeof result.added === "number"
        || typeof result.refreshed === "number"
        || typeof result.skipped === "number"
      )
        ? `新增 ${result.added ?? 0} 个，刷新 ${result.refreshed ?? 0} 个，跳过 ${result.skipped ?? 0} 个`
        : "";
      setMessage(result.message || importSummary || "流程已开始，请按页面提示继续操作");
      if (result.status === "error" && result.errors?.length) {
        setError(result.errors.join("；"));
      }
      if (channel === "chatgpt" && selectedFlow.id === "oauth-pkce" && result.authorize_url) {
        window.open(result.authorize_url, "_blank", "noopener,noreferrer");
      }
      if (result.status === "success" || result.status === "succeeded") {
        onComplete(`${selectedChannel.name} 账号新增成功，账号列表已更新`, channel);
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
      const completionPayload: Record<string, unknown> = {};
      if (channel === "wb" && typeof payload.region === "string" && payload.region.trim()) {
        completionPayload.region = payload.region.trim();
      }
      if (callback.trim()) completionPayload.callback = callback.trim();
      const result = await completeProvision(channel, session.session_id, {
        payload: completionPayload,
        idempotency_key: newIdempotencyKey(),
      });
      setSession((current) => ({ ...current, ...result }));
      if (result.status === "success" || result.status === "succeeded") {
        onComplete(`${selectedChannel?.name ?? channel} 账号新增成功，账号列表已更新`, channel);
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

  function selectChatGptImportMethod(method: ChatGptImportMethod) {
    setChatGptImportMethod(method);
    setFlowId(method === "oauth" ? "oauth-pkce" : method === "menu" ? "" : "token-import");
    setError("");
    setMessage("");
  }

  async function submitChatGptImport(accounts: Record<string, unknown>[], tokens: string[]) {
    const normalizedTokens = tokens.map((item) => item.trim()).filter(Boolean);
    const normalizedAccounts = accounts.filter((item) => tokenFromRecord(item));
    if (normalizedTokens.length === 0 && normalizedAccounts.length === 0) {
      setError("没有读取到可用的 ChatGPT 访问令牌");
      return;
    }
    setChatGptImportBusy(true);
    setError("");
    try {
      await startFlow({ tokens: normalizedTokens, accounts: normalizedAccounts });
    } finally {
      setChatGptImportBusy(false);
    }
  }

  async function importChatGptSession() {
    try {
      const parsed = JSON.parse(chatGptSessionInput) as unknown;
      if (!isRecord(parsed)) throw new Error("会话 JSON 必须是对象");
      const token = tokenFromRecord(parsed);
      if (!token) throw new Error("会话 JSON 中未找到 accessToken 字段");
      await submitChatGptImport([], [token]);
    } catch (cause: unknown) {
      setError(cause instanceof Error ? cause.message : "会话 JSON 解析失败");
    }
  }

  async function importChatGptCodex() {
    try {
      const parsed = JSON.parse(chatGptCodexInput) as unknown;
      const account = normalizeChatGptAccount(parsed);
      if (!account) throw new Error("Codex 认证 JSON 中未找到 access_token 字段");
      await submitChatGptImport([account], []);
    } catch (cause: unknown) {
      setError(cause instanceof Error ? cause.message : "Codex 认证 JSON 解析失败");
    }
  }

  async function readChatGptAccountFiles(files: FileList | null) {
    if (!files?.length) return;
    setChatGptImportBusy(true);
    setError("");
    try {
      const parsed = await Promise.all(Array.from(files).map(async (file) => {
        const value = JSON.parse(await file.text()) as unknown;
        return extractChatGptImportAccounts(value);
      }));
      const accounts = parsed.flat();
      const tokens = accounts.map(tokenFromRecord).filter(Boolean);
      const errorCount = parsed.filter((items) => items.length === 0).length;
      if (accounts.length === 0) throw new Error("账号 JSON 文件中未找到 access_token 字段");
      setPendingChatGptImport({
        accounts,
        tokens,
        fileName: Array.from(files).map((file) => file.name).join(", "),
        errorCount,
      });
    } catch (cause: unknown) {
      setPendingChatGptImport(null);
      setError(cause instanceof SyntaxError ? "JSON 文件无法解析，请确认内容完整" : cause instanceof Error ? cause.message : "账号 JSON 读取失败");
    } finally {
      setChatGptImportBusy(false);
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
  const flowNeedsCompletion = Boolean(session && channel === "chatgpt" && activeFlow?.supports?.complete);
  const isChatGptTokenImport = channel === "chatgpt" && selectedFlow?.id === "token-import";

  return (
    <div className="key-modal-backdrop account-onboarding-backdrop" role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget && !busy) onClose();
    }}>
      <section className="key-editor account-onboarding-dialog" role="dialog" aria-modal="true" aria-labelledby="account-onboarding-title">
        <div className="section-heading">
          <div><h2 id="account-onboarding-title">新增账号</h2><p>先选择渠道，再选择登录或导入方式；下面的字段会随方式变化。</p></div>
          <Button variant="unstyled" className="icon-close" aria-label="关闭" onClick={onClose} disabled={busy}>×</Button>
        </div>

        {error && <div className="notice notice-error" role="alert">{error}</div>}
        {message && <div className="notice notice-info" role="status">{message}</div>}
        {session && (session.errors?.length || typeof session.added === "number" || typeof session.refreshed === "number") && (
          <div className={`notice ${session.errors?.length ? "notice-warning" : "notice-info"}`} role={session.errors?.length ? "alert" : "status"}>
            {(typeof session.added === "number" || typeof session.refreshed === "number" || typeof session.skipped === "number") && (
              <div>处理结果：新增 {session.added ?? 0} 个，刷新 {session.refreshed ?? 0} 个，跳过 {session.skipped ?? 0} 个。</div>
            )}
            {session.errors?.length ? <ul>{session.errors.map((item, index) => <li key={`${index}-${item}`}>{item}</li>)}</ul> : null}
          </div>
        )}

        <div className="onboarding-form">
          <label><span>渠道</span><Select
            value={channel}
            onChange={setChannel}
            disabled={busy || channelsLoading || Boolean(session)}
            placeholder={channelsLoading ? "正在读取渠道配置…" : "请选择渠道"}
            options={[
              { group: "可新增账号", options: available.map((item) => ({ value: item.slug, label: channelLabel(item.slug, item.name) })) },
              {
                group: "需要先配置账号能力",
                options: unavailable.map((item) => ({ value: item.slug, label: `${channelLabel(item.slug, item.name)}（未配置）`, disabled: true })),
              },
            ]}
          /></label>

          {channelsLoading && <div className="onboarding-channel-status" role="status">正在读取渠道账号能力…</div>}
          {!channelsLoading && channelsError && <div className="notice notice-error onboarding-channel-notice" role="alert"><span>{channelsError}</span>{onRetryChannels && <Button variant="secondary" size="sm" onClick={onRetryChannels} disabled={busy}>重试</Button>}</div>}
          {!channelsLoading && !channelsError && channels.length === 0 && <div className="notice notice-warning onboarding-channel-notice" role="status">暂未读取到已注册渠道。</div>}
          {!channelsLoading && !channelsError && channels.length > 0 && available.length === 0 && <div className="notice notice-warning onboarding-channel-notice" role="status">当前没有已配置账号能力的渠道。</div>}
          {!channelsLoading && !channelsError && selectedChannel && <div className={`onboarding-channel-status ${(selectedChannel.provision_configured ?? selectedChannel.accounts_configured) ? "is-ready" : "is-unavailable"}`} role="status"><span className={`status-pill ${(selectedChannel.provision_configured ?? selectedChannel.accounts_configured) ? "success" : "warning"}`}><i />{channelStatusText(selectedChannel)}</span></div>}

          {channel && schemaLoading && <div className="table-state" role="status">正在读取该渠道的新增方式…</div>}
          {channel && !schemaLoading && schemaError && <div className="notice notice-error" role="alert">读取渠道新增能力失败：{schemaError}</div>}
          {channel && !schemaLoading && !schemaError && schema && availableFlows.length === 0 && <div className="notice notice-warning" role="status">该渠道当前没有可用的账号新增方式。</div>}
          {channel !== "chatgpt" && availableFlows.length > 0 && <label><span>新增方式</span><Select value={flowId} onChange={setFlowId} disabled={busy || Boolean(session)} placeholder="请选择新增方式" options={availableFlows.map((flow) => ({ value: flow.id, label: flowPresentation(channel, flow).title }))} /></label>}
          {activeFlow && <p className="secondary-text">{flowPresentation(channel, activeFlow).description}</p>}

          {channel === "chatgpt" && !session && <div className="onboarding-chatgpt-import">
            <span className="onboarding-subheading">选择新增方式</span>
            {chatGptImportMethod === "menu" && <div className="onboarding-method-grid">
              <ChatGptMethodCard title="网页登录已有账号" description="打开 ChatGPT 登录页，完成登录后粘贴回调地址。" icon={LogIn} onClick={() => selectChatGptImportMethod("oauth")} />
              <ChatGptMethodCard title="导入访问令牌" description="直接粘贴多个访问令牌，也可以从 TXT 文件读取。" icon={KeyRound} onClick={() => selectChatGptImportMethod("token")} />
              <ChatGptMethodCard title="导入会话 JSON" description="粘贴会话接口返回的 JSON，自动提取 accessToken。" icon={FileJson} onClick={() => selectChatGptImportMethod("session")} />
              <ChatGptMethodCard title="导入 Codex 认证 JSON" description="保留 access、refresh、id token 等完整认证字段。" icon={FileJson} onClick={() => selectChatGptImportMethod("codex")} />
              <ChatGptMethodCard title="导入账号 JSON 文件" description="支持单个账号、账号数组和多文件导入。" icon={Files} onClick={() => selectChatGptImportMethod("account-json")} />
            </div>}
            {chatGptImportMethod !== "menu" && <div className="onboarding-import-panel">
              <Button variant="unstyled" className="onboarding-back-button" onClick={() => { setChatGptImportMethod("menu"); setFlowId(""); setError(""); setMessage(""); }}><ArrowLeft size={15} />返回新增方式</Button>
              {chatGptImportMethod === "oauth" && <p className="secondary-text">点击“开始授权”后会打开 ChatGPT 登录页，完成登录后把浏览器地址栏中的回调地址粘贴到下方。</p>}
              {chatGptImportMethod === "token" && <>
                <label><span>访问令牌列表</span><textarea value={chatGptTokenInput} onChange={(event) => setChatGptTokenInput(event.target.value)} rows={5} placeholder="每行填写一个访问令牌" disabled={chatGptImportBusy} /></label>
                <label className="onboarding-file-picker"><span><FileText size={15} />选择 TXT 文件</span><input type="file" accept=".txt,text/plain" onChange={(event) => { const file = event.target.files?.[0]; event.currentTarget.value = ""; if (file) void file.text().then((value) => setChatGptTokenInput((current) => [...splitTokens(current), ...splitTokens(value)].join("\n"))).catch(() => setError("TXT 文件读取失败")); }} disabled={chatGptImportBusy} /></label>
                <Button variant="primary" className="primary-action-button" onClick={() => void submitChatGptImport([], splitTokens(chatGptTokenInput))} disabled={chatGptImportBusy || !splitTokens(chatGptTokenInput).length}>{chatGptImportBusy ? <LoaderCircle size={15} className="spin" /> : <Upload size={15} />}导入访问令牌</Button>
              </>}
              {chatGptImportMethod === "session" && <>
                <label><span>会话 JSON</span><textarea value={chatGptSessionInput} onChange={(event) => setChatGptSessionInput(event.target.value)} rows={7} placeholder='粘贴包含 "accessToken" 字段的完整 JSON' disabled={chatGptImportBusy} /></label>
                <Button variant="primary" className="primary-action-button" onClick={() => void importChatGptSession()} disabled={chatGptImportBusy || !chatGptSessionInput.trim()}><FileJson size={15} />导入 JSON</Button>
              </>}
              {chatGptImportMethod === "codex" && <>
                <label><span>Codex 认证 JSON</span><textarea value={chatGptCodexInput} onChange={(event) => setChatGptCodexInput(event.target.value)} rows={7} placeholder='粘贴包含 "access_token"、"refresh_token"、"id_token" 的 JSON' disabled={chatGptImportBusy} /></label>
                <Button variant="primary" className="primary-action-button" onClick={() => void importChatGptCodex()} disabled={chatGptImportBusy || !chatGptCodexInput.trim()}><FileJson size={15} />导入 JSON</Button>
              </>}
              {chatGptImportMethod === "account-json" && <>
                <label className="onboarding-file-picker onboarding-file-picker-large"><span><Files size={17} />选择一个或多个账号 JSON 文件</span><input type="file" accept=".json,application/json" multiple onChange={(event) => { void readChatGptAccountFiles(event.target.files); event.currentTarget.value = ""; }} disabled={chatGptImportBusy} /></label>
                {pendingChatGptImport && <div className="onboarding-import-summary"><strong>{pendingChatGptImport.fileName}</strong><span>识别到 {pendingChatGptImport.accounts.length} 个账号{pendingChatGptImport.errorCount ? `，${pendingChatGptImport.errorCount} 个文件未识别` : ""}</span><Button variant="primary" className="primary-action-button" onClick={() => void submitChatGptImport(pendingChatGptImport.accounts, pendingChatGptImport.tokens)} disabled={chatGptImportBusy}>确认导入</Button></div>}
              </>}
            </div>}
          </div>}

          {activeFlow && Object.entries(activeFlow.schema.properties ?? {}).map(([name, field]) => {
            if (name === "account_id") return null;
            if (isChatGptTokenImport) return null;
            if (name === "callback" && activeFlow.supports?.complete) return null;
            const value = payload[name];
            const label = fieldLabel(name, field);
            const description = fieldDescription(name, field);
            const required = activeFlow.schema.required?.includes(name) ?? false;
            if (field.enum?.length) return <label key={name}><span>{label}{required ? " *" : ""}</span><Select value={String(value ?? "")} onChange={(selected) => setField(name, field, selected)} disabled={busy || Boolean(session)} placeholder="请选择" options={field.enum.map((option) => ({ value: String(option), label: enumOptionLabel(name, option) }))} /><small>{description}</small></label>;
            if (field.type === "boolean") return <label key={name} className="checkbox-label"><input type="checkbox" checked={Boolean(value)} onChange={(event) => setPayload((current) => ({ ...current, [name]: event.target.checked }))} disabled={busy || Boolean(session)} /><span>{label}{required ? " *" : ""}</span><small>{description}</small></label>;
            return <label key={name}><span>{label}{required ? " *" : ""}</span>{isMultiline(field) ? <textarea value={stringifyFieldValue(value, field)} onChange={(event) => setField(name, field, event.target.value)} rows={4} disabled={busy || Boolean(session)} /> : <input type={field.secret || field.format === "password" ? "password" : field.type === "number" || field.type === "integer" ? "number" : "text"} value={stringifyFieldValue(value, field)} onChange={(event) => setField(name, field, event.target.value)} minLength={field.minLength} maxLength={field.maxLength} disabled={busy || Boolean(session)} />}{<small>{description}</small>}</label>;
          })}

          {session && (qrValue || qrImage) && <div className="onboarding-authorize-box">
            {authUrl && <a href={authUrl} target="_blank" rel="noreferrer" className="onboarding-url">{authUrl}</a>}
            {qrImage ? <img className="onboarding-qr-image" src={qrImage} alt="账号授权二维码" /> : <QRCodeSVG value={qrValue} size={210} level="M" />}
            <span className="onboarding-session-status">{sessionStatusText(session.status)}</span>
          </div>}
          {session && !qrValue && !qrImage && <div className="onboarding-authorize-box"><span className="onboarding-session-status">{sessionStatusText(session.status)}</span></div>}
          {flowNeedsCompletion && session && <label><span>授权回调地址</span><textarea value={callback} onChange={(event) => setCallback(event.target.value)} rows={3} placeholder="粘贴浏览器地址栏中的完整回调地址" disabled={busy} /><small>登录完成后复制浏览器地址栏内容；这是完成授权所必需的信息。</small></label>}
        </div>

        <div className="log-filter-actions onboarding-actions">
          {!session && !isChatGptTokenImport && <Button variant="primary" onClick={() => void startFlow()} disabled={busy || !canStart}>{busy ? "处理中…" : selectedFlow?.supports?.import ? "导入账号" : "开始授权"}</Button>}
          {flowNeedsCompletion && session && <Button variant="primary" onClick={() => void finishFlow()} disabled={busy}>{busy ? "提交中…" : "完成授权"}</Button>}
          {session && activeFlow?.supports?.cancel && !terminalStatuses.has(session.status ?? "") && <Button variant="secondary" onClick={() => void cancelFlow()} disabled={busy}>取消流程</Button>}
          {session && (error || terminalStatuses.has(session.status ?? "")) && <Button variant="secondary" onClick={() => { setSession(null); setMessage(""); setError(""); }} disabled={busy}>重新开始</Button>}
          <Button variant="secondary" onClick={onClose} disabled={busy}>关闭</Button>
        </div>
        {unavailable.length > 0 && <p className="onboarding-channel-footnote">标记为“未配置”的渠道需要先在服务端补齐账号能力配置。</p>}
      </section>
    </div>
  );
}
