import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { useToast } from "../../app/ToastContext"
import { fetchModels, refreshModels, type ModelRecord } from "../models/modelsApi";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import {
  clearPlaygroundConversations,
  deletePlaygroundConversation,
  fetchPlaygroundConversation,
  fetchPlaygroundConversations,
  fetchPlaygroundRuns,
  runPlaygroundGeneration,
  runPlaygroundFileTask,
  runPlaygroundSearch,
  runPlaygroundStream,
  isPlaygroundSuccess,
  type PlaygroundConversationSummary,
  type PlaygroundResult,
  type PlaygroundRun,
} from "./managementApi";
import { PlaygroundComposer } from "./playground/PlaygroundComposer";
import { PlaygroundConversation } from "./playground/PlaygroundConversation";
import { PlaygroundHistorySidebar } from "./playground/PlaygroundHistorySidebar";
import type {
  ChatMessage,
  ComposerMode,
  PlaygroundGenerationSettings,
  PlaygroundSidebarTab,
} from "./playground/types";
import "./PlaygroundPage.css";

function stripThinkMarkup(value: string): string {
  return value
    .replace(/<think\b[^>]*>[\s\S]*?<\/think\s*>/gi, "")
    .replace(/<\/?think\b[^>]*>/gi, "");
}

function mediaResultSummary(value: unknown): string {
  if (!value || typeof value !== "object") return "";
  const data = (value as { data?: unknown }).data;
  if (!Array.isArray(data)) return "";
  const media = data.filter((item) => {
    if (!item || typeof item !== "object") return false;
    const entry = item as { url?: unknown; video_url?: unknown; b64_json?: unknown };
    return typeof entry.url === "string"
      || typeof entry.video_url === "string"
      || typeof entry.b64_json === "string";
  });
  if (media.length === 0) return "";
  const isVideo = media.some((item) => typeof (item as { video_url?: unknown }).video_url === "string");
  return `已生成 ${media.length} 个${isVideo ? "视频" : "图片"}结果。`;
}

function responseContent(value: unknown): string {
  if (typeof value === "string") {
    const trimmed = stripThinkMarkup(value).trim();
    if (!trimmed) return "（空响应）";
    const isSse = trimmed.split(/\r?\n/).some((line) => line.trim().startsWith("data:"));
    if (isSse) {
      const chunks = trimmed
        .split(/\r?\n/)
        .filter((line) => line.trim().startsWith("data:"))
        .map((line) => line.slice(line.indexOf(":") + 1).trim())
        .filter((chunk) => chunk && chunk !== "[DONE]")
        .map((chunk) => {
          try {
            return responseContent(JSON.parse(chunk));
          } catch {
            return chunk;
          }
        })
        .filter((chunk) => chunk && chunk !== "（空响应）")
        .join("");
      return chunks || "（空响应）";
    }
    if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
      try {
        const parsed = responseContent(JSON.parse(trimmed) as unknown);
        if (parsed !== "（空响应）") return parsed;
      } catch {
        // Plain text can legitimately begin with a brace.
      }
    }
    return trimmed;
  }
  if (value === null || value === undefined) return "（空响应）";
  if (Array.isArray(value)) {
    const text = value.map(responseContent).filter((item) => item && item !== "（空响应）").join("");
    return text || "（空响应）";
  }
  if (typeof value !== "object") return String(value);

  const payload = value as Record<string, unknown>;
  const mediaSummary = mediaResultSummary(value);
  if (mediaSummary) return mediaSummary;
  const choices = Array.isArray(payload.choices) ? payload.choices : [];
  const choiceText = choices.map(responseContent).filter((item) => item && item !== "（空响应）").join("");
  if (choiceText) return choiceText;
  for (const key of ["message", "delta", "output_text", "text", "content"]) {
    if (payload[key] !== undefined) {
      const text = responseContent(payload[key]);
      if (text !== "（空响应）") return text;
    }
  }
  for (const key of ["output", "data"]) {
    if (typeof payload[key] === "string") return stripThinkMarkup(payload[key]);
    if (payload[key] !== undefined && (typeof payload[key] === "object" || Array.isArray(payload[key]))) {
      const text = responseContent(payload[key]);
      if (text !== "（空响应）") return text;
    }
  }
  return "（空响应）";
}

function responseError(value: unknown): string {
  if (!value || typeof value !== "object") return "";
  const error = (value as { error?: unknown }).error;
  if (typeof error === "string") return error;
  if (error && typeof error === "object" && typeof (error as { message?: unknown }).message === "string") {
    return (error as { message: string }).message;
  }
  return "";
}

function generationSummary(value: unknown, capability: "image" | "video"): string {
  if (value && typeof value === "object") {
    const data = (value as { data?: unknown }).data;
    if (Array.isArray(data)) {
      return `已生成 ${data.length || 1} 个${capability === "image" ? "图片" : "视频"}结果。`;
    }
  }
  return `已完成${capability === "image" ? "生图" : "视频生成"}请求。`;
}

function ratioToSize(ratio: string): string {
  const sizes: Record<string, string> = {
    "9:16": "1024x1536",
    "2:3": "1024x1536",
    "3:4": "1024x1365",
    "4:3": "1365x1024",
    "3:2": "1536x1024",
    "16:9": "1536x864",
    "1:1": "1024x1024",
  };
  return sizes[ratio] ?? "1024x1024";
}

function playgroundFailureMessage(result: PlaygroundResult): string {
  if (isPlaygroundSuccess(result.status, result.response_status)) return "";
  const status = result.response_status ? `上游 HTTP ${result.response_status}` : "上游未返回有效状态";
  const detail = responseError(result.response);
  return detail ? `调试请求失败（${status}）：${detail}` : `调试请求失败（${status}）`;
}

function createMessageId(): string {
  return `message-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function restoreConversationMessage(item: {
  id: string;
  role: "user" | "assistant" | "error";
  content: string;
  model?: string;
  raw?: unknown;
}): ChatMessage {
  const content = item.raw !== undefined ? responseContent(item.raw) : item.content.trim() || "（空响应）";
  if (item.role === "assistant" && content === "（空响应）") {
    return {
      id: item.id,
      role: "error",
      content: "上游返回了空内容，该请求未生成有效回答，请重新发送。",
      model: item.model,
      raw: item.raw,
    };
  }
  return { id: item.id, role: item.role, content, model: item.model, raw: item.raw };
}

type PlaygroundSessionState = {
  conversationId: string | null;
  channel: string;
  model: string;
  messages: ChatMessage[];
  response: unknown;
};

const EMPTY_SESSION: PlaygroundSessionState = {
  conversationId: null,
  channel: "",
  model: "",
  messages: [],
  response: null,
};

const DEFAULT_GENERATION_SETTINGS: PlaygroundGenerationSettings = {
  imageRatio: "1:1",
  videoRatio: "16:9",
  videoDuration: 10,
};

export function PlaygroundPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const toast = useToast();
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [models, setModels] = useState<ModelRecord[]>([]);
  const [conversations, setConversations] = useState<PlaygroundConversationSummary[]>([]);
  const [sidebarTab, setSidebarTab] = useState<PlaygroundSidebarTab>("conversations");
  const [requestRuns, setRequestRuns] = useState<PlaygroundRun[]>([]);
  const [requestPage, setRequestPage] = useState(1);
  const [requestPageSize, setRequestPageSize] = useState(20);
  const [requestTotal, setRequestTotal] = useState(0);
  const [requestLoading, setRequestLoading] = useState(false);
  const [page, setPage] = useState(1);
  const [conversationPageSize, setConversationPageSize] = useState(20);
  const [total, setTotal] = useState(0);
  const [historyLoading, setHistoryLoading] = useState(true);
  const [conversationLoading, setConversationLoading] = useState(false);
  const [session, setSession] = useState<PlaygroundSessionState>(EMPTY_SESSION);
  const [composerMode, setComposerMode] = useState<ComposerMode>("chat");
  const [generationSettings, setGenerationSettings] = useState<PlaygroundGenerationSettings>(DEFAULT_GENERATION_SETTINGS);
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const [modelsLoading, setModelsLoading] = useState(false);
  const [modelsRetry, setModelsRetry] = useState(0);
  const [historyReload, setHistoryReload] = useState(0);
  const [requestReload, setRequestReload] = useState(0);
  const [copied, setCopied] = useState(false);
  const [deletingConversationId, setDeletingConversationId] = useState<string | null>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const streamAbortRef = useRef<AbortController | null>(null);
  const conversationLoadAbortRef = useRef<AbortController | null>(null);
  const submitAbortRef = useRef<AbortController | null>(null);
  const historyRequestRef = useRef(0);
  const requestListRequestRef = useRef(0);
  const sessionRevisionRef = useRef(0);
  const modelsRequestRef = useRef(0);

  const { conversationId, channel, model, messages, response } = session;

  useEffect(() => () => {
    streamAbortRef.current?.abort();
    submitAbortRef.current?.abort();
    conversationLoadAbortRef.current?.abort();
  }, []);

  useEffect(() => {
    const transcript = transcriptRef.current;
    if (transcript) transcript.scrollTop = transcript.scrollHeight;
  }, [messages, busy]);

  useEffect(() => {
    const controller = new AbortController();
    const initialSessionRevision = sessionRevisionRef.current;
    fetchChannels(controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return;
        setChannels(result);
        if (sessionRevisionRef.current === initialSessionRevision) {
          setSession((current) => {
            if (current.channel) return current;
            return { ...current, channel: result.find((item) => item.enabled)?.slug ?? result[0]?.slug ?? "" };
          });
        }
        setModelsRetry((value) => value + 1);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) toast.error(cause instanceof ApiClientError ? cause.message : "读取渠道失败");
      });
    return () => controller.abort();
  }, [toast]);

  useEffect(() => {
    const controller = new AbortController();
    setHistoryLoading(true);
    const requestId = ++historyRequestRef.current;
    fetchPlaygroundConversations(page, conversationPageSize, controller.signal)
      .then((result) => {
        if (controller.signal.aborted || requestId !== historyRequestRef.current) return;
        setConversations(result.data);
        setTotal(result.pagination.total);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted && requestId === historyRequestRef.current) {
          toast.error(cause instanceof ApiClientError ? cause.message : "读取对话历史失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted && requestId === historyRequestRef.current) setHistoryLoading(false);
      });
    return () => controller.abort();
  }, [page, conversationPageSize, historyReload, toast]);

  useEffect(() => {
    if (sidebarTab !== "requests") return undefined;
    const controller = new AbortController();
    setRequestLoading(true);
    const requestId = ++requestListRequestRef.current;
    fetchPlaygroundRuns(requestPage, requestPageSize, controller.signal)
      .then((result) => {
        if (controller.signal.aborted || requestId !== requestListRequestRef.current) return;
        setRequestRuns(result.data);
        setRequestTotal(result.pagination.total);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted && requestId === requestListRequestRef.current) {
          toast.error(cause instanceof ApiClientError ? cause.message : "读取请求记录失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted && requestId === requestListRequestRef.current) setRequestLoading(false);
      });
    return () => controller.abort();
  }, [sidebarTab, requestPage, requestPageSize, requestReload, toast]);

  useEffect(() => {
    const requestId = ++modelsRequestRef.current;
    const sessionRevisionAtStart = sessionRevisionRef.current;
    if (!channel) {
      setModels([]);
      setSession((current) => ({ ...current, model: "" }));
      setModelsLoading(false);
      return undefined;
    }
    const controller = new AbortController();
    setModelsLoading(true);
    const loadModels = async () => {
      let result = await fetchModels(1, { channel, enabled: true }, controller.signal, 200);
      if (role === "admin") {
        try {
          await refreshModels(channel);
          if (controller.signal.aborted || requestId !== modelsRequestRef.current) return;
          result = await fetchModels(1, { channel, enabled: true }, controller.signal, 200);
        } catch (cause: unknown) {
          // Keep an observed cache usable when a provider is temporarily
          // unavailable; a missing cache still surfaces the refresh error.
          if (result.data.length === 0) throw cause;
        }
      }
      if (controller.signal.aborted || requestId !== modelsRequestRef.current) return;
      setModels(result.data);
      if (sessionRevisionRef.current === sessionRevisionAtStart) {
        setSession((current) => ({
          ...current,
          model: result.data.some((item) => item.upstream_id === current.model)
            ? current.model
            : result.data[0]?.upstream_id ?? "",
        }));
      }
    };
    loadModels()
      .catch((cause: unknown) => {
        if (!controller.signal.aborted && requestId === modelsRequestRef.current) {
          setModels([]);
          if (sessionRevisionRef.current === sessionRevisionAtStart) {
            setSession((current) => ({ ...current, model: "" }));
          }
          toast.error(cause instanceof ApiClientError ? cause.message : "读取模型广场失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted && requestId === modelsRequestRef.current) setModelsLoading(false);
      });
    return () => controller.abort();
  }, [channel, modelsRetry, role, toast]);

  const selectedChannel = useMemo(() => channels.find((item) => item.slug === channel), [channels, channel]);
  const modeCapability = composerMode === "image" || composerMode === "video" ? composerMode : "chat";
  const modeModels = useMemo(
    () => models.filter((item) => item.caps.includes(modeCapability)),
    [models, modeCapability],
  );
  const availableModes = useMemo<ComposerMode[]>(
    () => [
      ...(models.some((item) => item.caps.includes("chat")) ? ["chat" as const] : []),
      ...(models.some((item) => item.caps.includes("image")) ? ["image" as const] : []),
      ...(models.some((item) => item.caps.includes("video")) ? ["video" as const] : []),
    ],
    [models],
  );
  const selectedModel = useMemo(() => modeModels.find((item) => item.upstream_id === model), [modeModels, model]);
  const modelLabel = selectedModel?.display_name ?? model;
  function invalidateActiveRequest() {
    sessionRevisionRef.current += 1;
    submitAbortRef.current?.abort();
    streamAbortRef.current?.abort();
    setBusy(false);
  }

  function handleChannelChange(nextChannel: string) {
    invalidateActiveRequest();
    setSession((current) => ({ ...current, channel: nextChannel, model: "" }));
    setComposerMode("chat");
  }

  function handleModeChange(nextMode: ComposerMode) {
    invalidateActiveRequest();
    setComposerMode(nextMode);
    const capability = nextMode === "image" || nextMode === "video" ? nextMode : "chat";
    const nextModel = models.find((item) => item.caps.includes(capability));
    setSession((current) => ({ ...current, model: nextModel?.upstream_id ?? "" }));
  }

  function handleModelChange(nextModel: string) {
    invalidateActiveRequest();
    setSession((current) => ({ ...current, model: nextModel }));
  }

  useEffect(() => {
    if (availableModes.length === 0) {
      setSession((current) => ({ ...current, model: "" }));
      return;
    }
    if (!availableModes.includes(composerMode)) {
      setComposerMode("chat");
      return;
    }
    if (!modeModels.some((item) => item.upstream_id === model)) {
      setSession((current) => ({ ...current, model: modeModels[0]?.upstream_id ?? "" }));
    }
  }, [availableModes, composerMode, modeModels, model]);

  function startNewConversation() {
    sessionRevisionRef.current += 1;
    streamAbortRef.current?.abort();
    submitAbortRef.current?.abort();
    conversationLoadAbortRef.current?.abort();
    setSession(EMPTY_SESSION);
    setSidebarTab("conversations");
    setMessage("");
    setBusy(false);
    setConversationLoading(false);
    setCopied(false);
  }

  async function openConversation(id: string) {
    if (id === conversationId && !conversationLoading) return;
    conversationLoadAbortRef.current?.abort();
    streamAbortRef.current?.abort();
    submitAbortRef.current?.abort();
    const controller = new AbortController();
    conversationLoadAbortRef.current = controller;
    const requestRevision = ++sessionRevisionRef.current;
    setConversationLoading(true);
    setBusy(false);
    try {
      const conversation = await fetchPlaygroundConversation(id, controller.signal);
      if (controller.signal.aborted || requestRevision !== sessionRevisionRef.current) return;
      const conversationMessages = conversation.messages ?? [];
      const restoredMessages = conversationMessages.map(restoreConversationMessage);
      const lastRaw = [...conversationMessages].reverse().find((item) => item.raw !== undefined)?.raw ?? null;
      setSession({
        conversationId: conversation.id,
        channel: conversation.channel,
        model: conversation.model,
        messages: restoredMessages,
        response: lastRaw,
      });
      setMessage("");
      setCopied(false);
    } catch (cause: unknown) {
      if (!controller.signal.aborted && requestRevision === sessionRevisionRef.current) {
        toast.error(cause instanceof ApiClientError ? cause.message : "读取对话历史失败");
      }
    } finally {
      if (conversationLoadAbortRef.current === controller) conversationLoadAbortRef.current = null;
      if (!controller.signal.aborted && requestRevision === sessionRevisionRef.current) setConversationLoading(false);
    }
  }

  async function clearConversations() {
    if (role !== "admin" || busy || deletingConversationId) return;
    if (!window.confirm("确定清空当前账号的全部对话历史及请求记录吗？此操作不可撤销。")) return;
    setDeletingConversationId("__all__");
    try {
      await clearPlaygroundConversations();
      startNewConversation();
      setConversations([]);
      setTotal(0);
      setPage(1);
      setHistoryReload((value) => value + 1);
      setRequestReload((value) => value + 1);
      toast.success("对话历史已清空");
    } catch (cause: unknown) {
      toast.error(cause instanceof ApiClientError ? cause.message : "清空对话历史失败");
    } finally {
      setDeletingConversationId(null);
    }
  }

  async function deleteConversation(id: string, title: string) {
    if (role !== "admin" || busy || deletingConversationId) return;
    if (!window.confirm(`确定删除对话“${title}”及其请求记录吗？`)) return;
    setDeletingConversationId(id);
    try {
      await deletePlaygroundConversation(id);
      setConversations((current) => current.filter((item) => item.id !== id));
      setTotal((current) => Math.max(0, current - 1));
      if (conversationId === id) startNewConversation();
      if (conversations.length === 1 && page > 1) setPage((current) => current - 1);
      else setHistoryReload((value) => value + 1);
      setRequestReload((value) => value + 1);
      toast.success("对话已删除");
    } catch (cause: unknown) {
      toast.error(cause instanceof ApiClientError ? cause.message : "删除对话失败");
    } finally {
      setDeletingConversationId(null);
    }
  }

  function handleComposerKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      event.currentTarget.form?.requestSubmit();
    }
  }

  async function submit(event: FormEvent<HTMLFormElement>, images: string[]) {
    event.preventDefault();
    const selected = modeModels.find((item) => item.upstream_id === model);
    const prompt = message.trim();
    if (role !== "admin" || !channel || !selected || !prompt || busy || (composerMode === "psd" && images.length === 0)) return;

    const submitRevision = ++sessionRevisionRef.current;
    submitAbortRef.current?.abort();
    const submitController = new AbortController();
    submitAbortRef.current = submitController;
    const isCurrent = () => !submitController.signal.aborted && submitRevision === sessionRevisionRef.current;
    const contextMessages = messages
      .filter((item): item is ChatMessage & { role: "user" | "assistant" } => item.role !== "error")
      .map((item) => ({ role: item.role, content: item.content }));
    const assistantId = createMessageId();
    setSession((current) => ({
      ...current,
      messages: [
        ...current.messages,
        { id: createMessageId(), role: "user", content: prompt, model: selected.upstream_id },
      ],
      response: null,
    }));
    setMessage("");
    setBusy(true);

    const appendAssistant = (next: ChatMessage) => {
      if (!isCurrent()) return;
      setSession((current) => ({ ...current, messages: [...current.messages, next] }));
    };
    const updateAssistant = (updater: (item: ChatMessage) => ChatMessage) => {
      if (!isCurrent()) return;
      setSession((current) => ({
        ...current,
        messages: current.messages.map((item) => item.id === assistantId ? updater(item) : item),
      }));
    };
    const setResult = (nextResponse: unknown, nextConversationId?: string) => {
      if (!isCurrent()) return;
      setSession((current) => ({
        ...current,
        conversationId: nextConversationId || current.conversationId,
        response: nextResponse,
      }));
    };

    if (composerMode === "image" || composerMode === "video") {
      try {
        const ratio = composerMode === "image"
          ? generationSettings.imageRatio === "自动" ? "" : generationSettings.imageRatio
          : generationSettings.videoRatio === "自动" ? "" : generationSettings.videoRatio;
        const result = await runPlaygroundGeneration({
          channel,
          model: selected.upstream_id,
          capability: composerMode,
          prompt,
          size: composerMode === "image" ? ratioToSize(ratio) : undefined,
          ratio,
          duration: generationSettings.videoDuration,
          base64_images: images,
        }, submitController.signal);
        if (!isCurrent()) return;
        const failed = !isPlaygroundSuccess(result.status, result.response_status);
        const content = failed
          ? responseError(result.response) || `生成失败（上游 HTTP ${result.response_status}）`
          : generationSummary(result.response, composerMode);
        setResult(result.response, result.conversation_id);
        appendAssistant({ id: assistantId, role: failed ? "error" : "assistant", content, model: selected.upstream_id, raw: result.response });
        if (failed) toast.error(content);
        else {
          toast.success(generationSummary(result.response, composerMode));
          setHistoryReload((value) => value + 1);
        }
      } catch (cause: unknown) {
        if (!isCurrent()) return;
        const detail = cause instanceof ApiClientError ? cause.message : "媒体生成请求失败";
        toast.error(detail);
        appendAssistant({ id: assistantId, role: "error", content: detail, model: selected.upstream_id });
      } finally {
        if (submitAbortRef.current === submitController) submitAbortRef.current = null;
        if (isCurrent()) setBusy(false);
      }
      return;
    }

    if (composerMode === "search" || composerMode === "ppt" || composerMode === "psd") {
      try {
        if (composerMode === "search") {
          const result = await runPlaygroundSearch({ channel, model: selected.upstream_id, prompt }, submitController.signal);
          if (!isCurrent()) return;
          const sources = result.sources.length > 0
            ? `\n\n来源：\n${result.sources.map((source) => `- [${source.title || source.url}](${source.url})`).join("\n")}`
            : "";
          setResult(result, result.conversation_id);
          appendAssistant({ id: assistantId, role: "assistant", content: `${result.answer || "（空响应）"}${sources}`, model: selected.upstream_id, raw: result });
          toast.success("搜索请求已完成");
          setHistoryReload((value) => value + 1);
        } else {
          const result = await runPlaygroundFileTask({
            channel,
            model: selected.upstream_id,
            kind: composerMode,
            prompt,
            base64_images: images,
          }, submitController.signal);
          if (!isCurrent()) return;
          const links = [
            result.primary_url ? `[下载${composerMode === "ppt" ? " PPT" : " PSD"}](${result.primary_url})` : "",
            result.zip_url ? `[下载素材包](${result.zip_url})` : "",
          ].filter(Boolean).join("\n");
          const failed = result.status !== "success";
          const content = failed ? result.error || "文件任务失败" : `文件已生成。${links ? `\n\n${links}` : ""}`;
          setResult(result);
          appendAssistant({ id: assistantId, role: failed ? "error" : "assistant", content, model: selected.upstream_id, raw: result });
          if (failed) toast.error(content);
          else {
            toast.success("文件已生成");
            setHistoryReload((value) => value + 1);
          }
        }
      } catch (cause: unknown) {
        if (!isCurrent()) return;
        const detail = cause instanceof ApiClientError ? cause.message : "调试请求失败";
        toast.error(detail);
        appendAssistant({ id: assistantId, role: "error", content: detail, model: selected.upstream_id });
      } finally {
        if (submitAbortRef.current === submitController) submitAbortRef.current = null;
        if (isCurrent()) setBusy(false);
      }
      return;
    }

    const runtimePrompt = composerMode === "writing"
      ? `请帮我写作：${prompt}`
      : composerMode === "quiz"
        ? `请解答并分析：${prompt}`
        : composerMode === "transcribe"
          ? `请执行录音转写：${prompt}`
          : prompt;
    streamAbortRef.current = submitController;
    appendAssistant({ id: assistantId, role: "assistant", content: "", model: selected.upstream_id });
    try {
      let streamError = "";
      const result = await runPlaygroundStream({
        channel,
        model: selected.upstream_id,
        conversation_id: conversationId ?? undefined,
        messages: [...contextMessages, { role: "user", content: runtimePrompt }],
      }, (streamEvent) => {
        if (!isCurrent()) return;
        if (streamEvent.type === "delta") {
          updateAssistant((item) => ({ ...item, content: item.content + stripThinkMarkup(streamEvent.content) }));
        } else if (streamEvent.type === "error") {
          streamError = streamEvent.message;
        }
      }, submitController.signal);
      if (!isCurrent()) return;
      const failure = playgroundFailureMessage(result);
      setResult(result.response, result.conversation_id);
      const finalError = streamError || failure;
      updateAssistant((item) => ({
        ...item,
        role: finalError ? "error" : "assistant",
        content: finalError || responseContent(result.response),
        raw: result.response,
      }));
      if (finalError) toast.error(finalError);
      else {
        toast.success("调试请求已完成");
        setHistoryReload((value) => value + 1);
      }
    } catch (cause: unknown) {
      if (!isCurrent()) return;
      const detail = cause instanceof ApiClientError ? cause.message : "调试请求失败";
      toast.error(detail);
      updateAssistant((item) => ({ ...item, role: "error", content: detail }));
    } finally {
      if (streamAbortRef.current === submitController) streamAbortRef.current = null;
      if (submitAbortRef.current === submitController) submitAbortRef.current = null;
      if (isCurrent()) setBusy(false);
    }
  }

  async function copyLatestResponse() {
    if (response === null || !navigator.clipboard) return;
    await navigator.clipboard.writeText(responseContent(response));
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1600);
  }

  const composer = <PlaygroundComposer
    role={role}
    channels={channels}
    channel={channel}
    model={model}
    selectedChannel={selectedChannel}
    selectedModel={selectedModel}
    availableModels={modeModels}
    availableModes={availableModes}
    generationSettings={generationSettings}
    modelsLoading={modelsLoading}
    message={message}
    busy={busy}
    mode={composerMode}
    onChannelChange={handleChannelChange}
    onModelChange={handleModelChange}
    onMessageChange={(event) => setMessage(event.target.value)}
    onKeyDown={handleComposerKeyDown}
    onModeChange={handleModeChange}
    onGenerationSettingsChange={setGenerationSettings}
    onSubmit={(event, images) => void submit(event, images)}
  />;

  return (
    <main className="page-content data-page management-page playground-page">
      <div className="playground-layout">
        <PlaygroundConversation
          modelLabel={modelLabel}
          channelLabel={selectedChannel?.name}
          busy={busy}
          messages={messages}
          transcriptRef={transcriptRef}
          onSuggestion={setMessage}
          composer={composer}
        />
        <PlaygroundHistorySidebar
          role={role}
          sidebarTab={sidebarTab}
          conversations={conversations}
          requestRuns={requestRuns}
          conversationId={conversationId}
          historyLoading={historyLoading}
          requestLoading={requestLoading}
          total={total}
          requestTotal={requestTotal}
          page={page}
          requestPage={requestPage}
          conversationPageSize={conversationPageSize}
          requestPageSize={requestPageSize}
          busy={busy}
          deletingConversationId={deletingConversationId}
          response={response}
          copied={copied}
          onStartNewConversation={startNewConversation}
          onClearConversations={() => void clearConversations()}
          onOpenConversation={(id) => void openConversation(id)}
          onDeleteConversation={(id, title) => void deleteConversation(id, title)}
          onSidebarTabChange={setSidebarTab}
          onReload={() => {
            if (sidebarTab === "conversations") setHistoryReload((value) => value + 1);
            else setRequestReload((value) => value + 1);
          }}
          onPageChange={setPage}
          onRequestPageChange={setRequestPage}
          onPageSizeChange={(nextPageSize) => {
            if (sidebarTab === "conversations") {
              setPage(1);
              setConversationPageSize(nextPageSize);
            } else {
              setRequestPage(1);
              setRequestPageSize(nextPageSize);
            }
          }}
          onCopyLatestResponse={() => void copyLatestResponse()}
        />
      </div>
    </main>
  );
}
