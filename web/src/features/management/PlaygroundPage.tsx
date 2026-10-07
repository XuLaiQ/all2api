import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { AlertCircle } from "lucide-react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { fetchModels, refreshModels, type ModelRecord } from "../models/modelsApi";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import {
  deletePlaygroundConversation,
  fetchPlaygroundConversation,
  fetchPlaygroundConversations,
  fetchPlaygroundRuns,
  runPlaygroundFileTask,
  runPlaygroundSearch,
  runPlaygroundStream,
  type PlaygroundConversationSummary,
  type PlaygroundResult,
  type PlaygroundRun,
} from "./managementApi";
import { PlaygroundComposer } from "./playground/PlaygroundComposer";
import { PlaygroundConversation } from "./playground/PlaygroundConversation";
import { PlaygroundHistorySidebar } from "./playground/PlaygroundHistorySidebar";
import type { ChatMessage, ComposerMode, PlaygroundSidebarTab } from "./playground/types";
import "./PlaygroundPage.css";

function stripThinkMarkup(value: string): string {
  return value
    .replace(/<think\b[^>]*>[\s\S]*?<\/think\s*>/gi, "")
    .replace(/<\/?think\b[^>]*>/gi, "");
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

function playgroundFailureMessage(result: PlaygroundResult): string {
  if (result.status === "ok" && result.response_status < 400) return "";
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

export function PlaygroundPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [models, setModels] = useState<ModelRecord[]>([]);
  const [conversations, setConversations] = useState<PlaygroundConversationSummary[]>([]);
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [sidebarTab, setSidebarTab] = useState<PlaygroundSidebarTab>("conversations");
  const [requestRuns, setRequestRuns] = useState<PlaygroundRun[]>([]);
  const [requestPage, setRequestPage] = useState(1);
  const [requestTotal, setRequestTotal] = useState(0);
  const [requestLoading, setRequestLoading] = useState(false);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [total, setTotal] = useState(0);
  const [historyLoading, setHistoryLoading] = useState(true);
  const [channel, setChannel] = useState("");
  const [model, setModel] = useState("");
  const [composerMode, setComposerMode] = useState<ComposerMode>("chat");
  const [message, setMessage] = useState("");
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [response, setResponse] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [modelsError, setModelsError] = useState("");
  const [modelsLoading, setModelsLoading] = useState(false);
  const [modelsRetry, setModelsRetry] = useState(0);
  const [reload, setReload] = useState(0);
  const [copied, setCopied] = useState(false);
  const [deletingConversationId, setDeletingConversationId] = useState<string | null>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const streamAbortRef = useRef<AbortController | null>(null);

  useEffect(() => () => streamAbortRef.current?.abort(), []);

  useEffect(() => {
    const transcript = transcriptRef.current;
    if (transcript) transcript.scrollTop = transcript.scrollHeight;
  }, [messages, busy]);

  useEffect(() => {
    const controller = new AbortController();
    setHistoryLoading(true);
    setError("");
    Promise.all([
      fetchChannels(controller.signal),
      fetchPlaygroundConversations(page, pageSize, controller.signal),
    ])
      .then(([channelResult, conversationResult]) => {
        setChannels(channelResult);
        setConversations(conversationResult.data);
        setTotal(conversationResult.pagination.total);
        setChannel((current) => channelResult.some((item) => item.slug === current)
          ? current
          : channelResult.find((item) => item.enabled)?.slug ?? channelResult[0]?.slug ?? "");
        setModelsRetry((value) => value + 1);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) setError(cause instanceof ApiClientError ? cause.message : "读取调试台数据失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setHistoryLoading(false);
      });
    return () => controller.abort();
  }, [page, pageSize, reload]);

  useEffect(() => {
    if (sidebarTab !== "requests") return undefined;
    const controller = new AbortController();
    setRequestLoading(true);
    fetchPlaygroundRuns(requestPage, pageSize, controller.signal)
      .then((result) => {
        setRequestRuns(result.data);
        setRequestTotal(result.pagination.total);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) setError(cause instanceof ApiClientError ? cause.message : "读取请求记录失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setRequestLoading(false);
      });
    return () => controller.abort();
  }, [sidebarTab, requestPage, pageSize, reload]);

  useEffect(() => {
    if (!channel) {
      setModels([]);
      setModel("");
      setModelsError("");
      setModelsLoading(false);
      return undefined;
    }
    const controller = new AbortController();
    setModelsLoading(true);
    setModelsError("");
    const loadModels = async () => {
      let result = await fetchModels(1, { channel, enabled: true }, controller.signal, 200);
      if (result.data.length === 0 && role === "admin") {
        await refreshModels(channel);
        if (controller.signal.aborted) return;
        result = await fetchModels(1, { channel, enabled: true }, controller.signal, 200);
      }
      if (controller.signal.aborted) return;
      setModels(result.data);
      setModel((current) => result.data.some((item) => item.upstream_id === current)
        ? current
        : result.data[0]?.upstream_id ?? "");
    };
    loadModels()
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setModels([]);
          setModel("");
          setModelsError(cause instanceof ApiClientError ? cause.message : "读取模型广场失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setModelsLoading(false);
      });
    return () => controller.abort();
  }, [channel, modelsRetry, role]);

  const selectedChannel = useMemo(() => channels.find((item) => item.slug === channel), [channels, channel]);
  const selectedModel = useMemo(() => models.find((item) => item.upstream_id === model), [models, model]);
  const modelLabel = selectedModel?.display_name ?? model;
  function handleChannelChange(nextChannel: string) {
    setChannel(nextChannel);
    setModel("");
  }

  function startNewConversation() {
    setConversationId(null);
    setSidebarTab("conversations");
    setMessages([]);
    setMessage("");
    setResponse(null);
    setError("");
    setCopied(false);
  }

  async function openConversation(id: string) {
    setHistoryLoading(true);
    setError("");
    try {
      const conversation = await fetchPlaygroundConversation(id);
      setConversationId(conversation.id);
      setChannel(conversation.channel);
      setModel(conversation.model);
      setMessages(conversation.messages.map(restoreConversationMessage));
      const lastRaw = [...conversation.messages].reverse().find((item) => item.raw !== undefined)?.raw;
      setResponse(lastRaw ?? null);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "读取对话历史失败");
    } finally {
      setHistoryLoading(false);
    }
  }

  async function deleteConversation(id: string, title: string) {
    if (role !== "admin" || busy || deletingConversationId) return;
    if (!window.confirm(`确定删除对话“${title}”及其请求记录吗？`)) return;
    setDeletingConversationId(id);
    setError("");
    try {
      await deletePlaygroundConversation(id);
      setConversations((current) => current.filter((item) => item.id !== id));
      setTotal((current) => Math.max(0, current - 1));
      if (conversationId === id) startNewConversation();
      if (conversations.length === 1 && page > 1) setPage((current) => current - 1);
      else setReload((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "删除对话失败");
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
    const selected = models.find((item) => item.upstream_id === model);
    const prompt = message.trim();
    if (role !== "admin" || !channel || !selected || !prompt || busy || (composerMode === "psd" && images.length === 0)) return;

    setMessages((current) => [...current, { id: createMessageId(), role: "user", content: prompt, model: selected.upstream_id }]);
    setMessage("");
    setBusy(true);
    setError("");
    setResponse(null);
    const assistantId = createMessageId();

    if (composerMode === "search" || composerMode === "ppt" || composerMode === "psd") {
      try {
        if (composerMode === "search") {
          const result = await runPlaygroundSearch({ channel, model: selected.upstream_id, prompt });
          const sources = result.sources.length > 0
            ? `\n\n来源：\n${result.sources.map((source) => `- [${source.title || source.url}](${source.url})`).join("\n")}`
            : "";
          const content = `${result.answer || "（空响应）"}${sources}`;
          setResponse(result);
          setMessages((current) => [...current, { id: assistantId, role: "assistant", content, model: selected.upstream_id, raw: result }]);
        } else {
          const result = await runPlaygroundFileTask({
            channel,
            model: selected.upstream_id,
            kind: composerMode,
            prompt,
            base64_images: images,
          });
          const links = [
            result.primary_url ? `[下载${composerMode === "ppt" ? " PPT" : " PSD"}](${result.primary_url})` : "",
            result.zip_url ? `[下载素材包](${result.zip_url})` : "",
          ].filter(Boolean).join("\n");
          const failed = result.status !== "success";
          const content = failed ? result.error || "文件任务失败" : `文件已生成。${links ? `\n\n${links}` : ""}`;
          setResponse(result);
          setMessages((current) => [...current, { id: assistantId, role: failed ? "error" : "assistant", content, model: selected.upstream_id, raw: result }]);
          if (failed) setError(content);
        }
        setReload((value) => value + 1);
      } catch (cause: unknown) {
        const detail = cause instanceof ApiClientError ? cause.message : "调试请求失败";
        setError(detail);
        setMessages((current) => [...current, { id: assistantId, role: "error", content: detail, model: selected.upstream_id }]);
      } finally {
        setBusy(false);
      }
      return;
    }

    const runtimePrompt = composerMode === "image"
      ? `请执行图像生成任务，使用当前图像参数完成：${prompt}`
      : composerMode === "video"
        ? `请执行视频生成任务，使用当前视频参数完成：${prompt}`
        : composerMode === "writing"
          ? `请帮我写作：${prompt}`
          : composerMode === "quiz"
            ? `请解答并分析：${prompt}`
            : composerMode === "transcribe"
              ? `请执行录音转写：${prompt}`
              : prompt;

    const streamController = new AbortController();
    streamAbortRef.current = streamController;
    setMessages((current) => [...current, { id: assistantId, role: "assistant", content: "", model: selected.upstream_id }]);
    try {
      const contextMessages = messages
        .filter((item): item is ChatMessage & { role: "user" | "assistant" } => item.role !== "error")
        .map((item) => ({ role: item.role, content: item.content }));
      let streamError = "";
      const result = await runPlaygroundStream({
        channel,
        model: selected.upstream_id,
        conversation_id: conversationId ?? undefined,
        messages: [...contextMessages, { role: "user", content: runtimePrompt }],
      }, (streamEvent) => {
        if (streamEvent.type === "delta") {
          setMessages((current) => current.map((item) => item.id === assistantId
            ? { ...item, content: item.content + stripThinkMarkup(streamEvent.content) }
            : item));
        } else if (streamEvent.type === "error") {
          streamError = streamEvent.message;
        }
      }, streamController.signal);
      const failure = playgroundFailureMessage(result);
      if (result.conversation_id) setConversationId(result.conversation_id);
      setResponse(result.response);
      const finalError = streamError || failure;
      setMessages((current) => current.map((item) => item.id === assistantId
        ? { ...item, role: finalError ? "error" : "assistant", content: finalError || responseContent(result.response), raw: result.response }
        : item));
      if (finalError) setError(finalError);
      else setReload((value) => value + 1);
    } catch (cause: unknown) {
      if (streamController.signal.aborted) return;
      const detail = cause instanceof ApiClientError ? cause.message : "调试请求失败";
      setError(detail);
      setMessages((current) => current.map((item) => item.id === assistantId ? { ...item, role: "error", content: detail } : item));
    } finally {
      if (streamAbortRef.current === streamController) streamAbortRef.current = null;
      setBusy(false);
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
    availableModels={models}
    modelsLoading={modelsLoading}
    message={message}
    busy={busy}
    mode={composerMode}
    onChannelChange={handleChannelChange}
    onModelChange={setModel}
    onMessageChange={(event) => setMessage(event.target.value)}
    onKeyDown={handleComposerKeyDown}
    onModeChange={setComposerMode}
    onSubmit={(event, images) => void submit(event, images)}
  />;

  return (
    <main className="page-content data-page management-page playground-page">
      {error && <div className="notice notice-error playground-notice" role="alert"><AlertCircle size={16} aria-hidden="true" /><span>{error}</span></div>}
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
          pageSize={pageSize}
          busy={busy}
          deletingConversationId={deletingConversationId}
          response={response}
          copied={copied}
          modelsError={modelsError}
          onStartNewConversation={startNewConversation}
          onOpenConversation={(id) => void openConversation(id)}
          onDeleteConversation={(id, title) => void deleteConversation(id, title)}
          onSidebarTabChange={setSidebarTab}
          onReload={() => setReload((value) => value + 1)}
          onPageChange={setPage}
          onRequestPageChange={setRequestPage}
          onPageSizeChange={(nextPageSize) => {
            setPage(1);
            setRequestPage(1);
            setPageSize(nextPageSize);
          }}
          onCopyLatestResponse={() => void copyLatestResponse()}
        />
      </div>
    </main>
  );
}
