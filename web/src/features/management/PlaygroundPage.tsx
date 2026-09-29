import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { AlertCircle, Bot, Check, Copy, FileJson, MessageCircle, Plus, RefreshCw, Send, Sparkles, Trash2, UserRound } from "lucide-react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { Pagination } from "../../app/data/Pagination";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "../../app/data/pagination.constants";
import { Select } from "../../app/controls/Select";
import { fetchModels, type ModelRecord } from "../models/modelsApi";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import {
  fetchPlaygroundConversation,
  fetchPlaygroundConversations,
  fetchPlaygroundRuns,
  deletePlaygroundConversation,
  runPlaygroundStream,
  type PlaygroundConversationSummary,
  type PlaygroundResult,
  type PlaygroundRun,
} from "./managementApi";

type ChatMessage = {
  id: string;
  role: "user" | "assistant" | "error";
  content: string;
  raw?: unknown;
};

function responseContent(value: unknown): string {
  if (typeof value === "string") {
    const trimmed = value.trim();
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
      if (chunks) return chunks;
      return "（空响应）";
    }
    if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
      try {
        const parsed = responseContent(JSON.parse(trimmed) as unknown);
        if (parsed !== "（空响应）") return parsed;
        return "（空响应）";
      } catch {
        // Plain text can legitimately begin with a brace; keep it as written.
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
  for (const key of ["message", "delta"]) {
    if (payload[key] !== undefined) {
      const text = responseContent(payload[key]);
      if (text !== "（空响应）") return text;
    }
  }
  for (const key of ["output_text", "text", "content"]) {
    if (payload[key] !== undefined) {
      const text = responseContent(payload[key]);
      if (text !== "（空响应）") return text;
    }
  }
  for (const key of ["output", "data"]) {
    if (typeof payload[key] === "string") return payload[key] as string;
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
  raw?: unknown;
}): ChatMessage {
  const content = item.raw !== undefined ? responseContent(item.raw) : item.content.trim() || "（空响应）";
  if (item.role === "assistant" && content === "（空响应）") {
    return {
      id: item.id,
      role: "error",
      content: "上游返回了空内容，该请求未生成有效回答，请重新发送。",
      raw: item.raw,
    };
  }
  return { id: item.id, role: item.role, content, raw: item.raw };
}

function runStatusLabel(run: PlaygroundRun): string {
  if (run.status === "ok" && (run.response_status ?? 500) < 400) return "已完成";
  if (run.status === "running") return "处理中";
  return "失败";
}

export function PlaygroundPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [models, setModels] = useState<ModelRecord[]>([]);
  const [conversations, setConversations] = useState<PlaygroundConversationSummary[]>([]);
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [sidebarTab, setSidebarTab] = useState<"conversations" | "requests">("conversations");
  const [requestRuns, setRequestRuns] = useState<PlaygroundRun[]>([]);
  const [requestPage, setRequestPage] = useState(1);
  const [requestTotal, setRequestTotal] = useState(0);
  const [requestLoading, setRequestLoading] = useState(false);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);
  const [historyLoading, setHistoryLoading] = useState(true);
  const [channel, setChannel] = useState("");
  const [model, setModel] = useState("");
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

  useEffect(() => () => {
    streamAbortRef.current?.abort();
  }, []);

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
      .then(([channelResult, runResult]) => {
        setChannels(channelResult);
        setConversations(runResult.data);
        setTotal(runResult.pagination.total);
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
    fetchModels(1, { channel, enabled: true }, controller.signal, 200)
      .then((result) => {
        setModels(result.data);
        setModel((current) => result.data.some((item) => item.upstream_id === current)
          ? current
          : result.data[0]?.upstream_id ?? "");
      })
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
  }, [channel, modelsRetry]);

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

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const selected = models.find((item) => item.upstream_id === model);
    const prompt = message.trim();
    if (role !== "admin" || !channel || !selected || !prompt || busy) return;

    setMessages((current) => [...current, { id: createMessageId(), role: "user", content: prompt }]);
    setMessage("");
    setBusy(true);
    setError("");
    setResponse(null);
    const assistantId = createMessageId();
    const streamController = new AbortController();
    streamAbortRef.current = streamController;
    setMessages((current) => [...current, { id: assistantId, role: "assistant", content: "" }]);
    try {
      const contextMessages = messages
        .filter((item): item is ChatMessage & { role: "user" | "assistant" } => item.role !== "error")
        .map((item) => ({ role: item.role, content: item.content }));
      let streamError = "";
      const result = await runPlaygroundStream({
        channel,
        model: selected.upstream_id,
        conversation_id: conversationId ?? undefined,
        messages: [...contextMessages, { role: "user", content: prompt }],
      }, (event) => {
        if (event.type === "delta") {
          setMessages((current) => current.map((item) => item.id === assistantId
            ? { ...item, content: item.content + event.content }
            : item));
        } else if (event.type === "error") {
          streamError = event.message;
        }
      }, streamController.signal);
      const failure = playgroundFailureMessage(result);
      if (result.conversation_id) setConversationId(result.conversation_id);
      setResponse(result.response);
      const finalError = streamError || failure;
      setMessages((current) => current.map((item) => item.id === assistantId
        ? {
            ...item,
            role: finalError ? "error" : "assistant",
            content: finalError || responseContent(result.response),
            raw: result.response,
          }
        : item));
      if (finalError) setError(finalError);
      else setReload((value) => value + 1);
    } catch (cause: unknown) {
      if (streamController.signal.aborted) return;
      const detail = cause instanceof ApiClientError ? cause.message : "调试请求失败";
      setError(detail);
      setMessages((current) => current.map((item) => item.id === assistantId
        ? { ...item, role: "error", content: detail }
        : item));
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

  return (
    <main className="page-content data-page management-page playground-page">
      <div className="page-heading playground-page-heading">
        <div>
          <span className="page-eyebrow">PLAYGROUND</span>
          <h1>调试台</h1>
          <p>选择渠道和模型，开始一段新的测试对话</p>
        </div>
        <button className="secondary-action-button" type="button" onClick={() => setReload((value) => value + 1)} disabled={historyLoading}>
          <RefreshCw size={15} aria-hidden="true" className={historyLoading ? "spin" : undefined} />
          刷新记录
        </button>
      </div>

      {error && <div className="notice notice-error playground-notice" role="alert"><AlertCircle size={16} aria-hidden="true" /><span>{error}</span></div>}

      <div className="playground-layout">
        <section className="playground-conversation" aria-label="调试对话">
          <header className="playground-toolbar">
            <div className="playground-context">
              <div className="playground-context-icon" aria-hidden="true"><Sparkles size={17} /></div>
              <div>
                <strong>{modelLabel || "新对话"}</strong>
                <span>{selectedChannel?.name ?? "等待选择渠道"} · 流式文本调试</span>
              </div>
            </div>
            <div className="playground-toolbar-controls">
              <label className="playground-select-field"><span>渠道</span><Select
                value={channel}
                onChange={handleChannelChange}
                disabled={role !== "admin"}
                placeholder="选择渠道"
                options={channels.map((item) => ({ value: item.slug, label: `${item.name} (${item.slug})` }))}
              /></label>
              <label className="playground-select-field"><span>模型</span><Select
                value={model}
                onChange={setModel}
                disabled={role !== "admin" || !channel || modelsLoading || models.length === 0}
                placeholder={modelsLoading ? "读取模型…" : !channel ? "先选渠道" : "选择模型"}
                options={models.map((item) => ({
                  value: item.upstream_id,
                  label: item.display_name === item.upstream_id ? item.display_name : `${item.display_name} (${item.upstream_id})`,
                }))}
              /></label>
              <button className="playground-new-button" type="button" onClick={startNewConversation} title="新建对话">
                <Plus size={16} aria-hidden="true" />
                新对话
              </button>
            </div>
          </header>

          <div className="playground-transcript" ref={transcriptRef} aria-live="polite">
            {messages.length === 0 ? (
              <div className="playground-empty-state">
                <div className="playground-empty-icon" aria-hidden="true"><MessageCircle size={24} /></div>
                <h2>准备开始对话</h2>
                <p>从模型广场选择一个可用模型，然后发送第一条消息。</p>
              </div>
            ) : messages.map((item) => (
              <article className={`playground-message is-${item.role}`} key={item.id}>
                <div className="playground-message-avatar" aria-hidden="true">
                  {item.role === "user" ? <UserRound size={16} /> : item.role === "error" ? <AlertCircle size={16} /> : <Bot size={17} />}
                </div>
                <div className="playground-message-content">
                  <div className="playground-message-name">{item.role === "user" ? "你" : item.role === "error" ? "调试台" : modelLabel || "助手"}</div>
                  <div className="playground-message-text">
                    {item.role === "assistant"
                      ? item.content
                        ? <ReactMarkdown remarkPlugins={[remarkGfm]}>{item.content}</ReactMarkdown>
                        : busy ? <span className="playground-stream-cursor" aria-hidden="true" /> : "（空响应）"
                      : item.content}
                  </div>
                  {item.raw !== undefined && <details className="playground-raw-message"><summary><FileJson size={14} aria-hidden="true" />查看原始响应</summary><pre>{JSON.stringify(item.raw, null, 2)}</pre></details>}
                </div>
              </article>
            ))}
            {busy && <div className="playground-typing" role="status"><span /><span /><span />正在生成回复…</div>}
          </div>

          <form className="playground-composer" onSubmit={(event) => void submit(event)}>
            <div className="playground-composer-box">
              <textarea
                value={message}
                onChange={(event) => setMessage(event.target.value)}
                onKeyDown={handleComposerKeyDown}
                rows={1}
                placeholder={role === "admin" ? "给模型发送消息…" : "当前角色仅可查看历史运行记录"}
                disabled={role !== "admin" || busy || !selectedModel}
                aria-label="消息"
              />
              <button className="playground-send-button" type="submit" disabled={role !== "admin" || busy || !channel || !selectedModel || !message.trim()} title="发送消息" aria-label="发送消息">
                <Send size={17} aria-hidden="true" />
              </button>
            </div>
            <div className="playground-composer-footer"><span>Enter 发送 · Shift + Enter 换行</span><span>{selectedModel ? `${selectedChannel?.name ?? channel} / ${model}` : "请选择可用模型"}</span></div>
          </form>
        </section>

        <aside className="playground-history" aria-label="调试台侧栏">
          <header className="playground-history-header">
            <div>
              <div className="playground-sidebar-tabs" role="tablist" aria-label="调试台侧栏视图">
                <button className={`playground-sidebar-tab${sidebarTab === "conversations" ? " is-active" : ""}`} type="button" role="tab" aria-selected={sidebarTab === "conversations"} onClick={() => setSidebarTab("conversations")}>对话历史</button>
                <button className={`playground-sidebar-tab${sidebarTab === "requests" ? " is-active" : ""}`} type="button" role="tab" aria-selected={sidebarTab === "requests"} onClick={() => setSidebarTab("requests")}>请求记录</button>
              </div>
              <span>{sidebarTab === "conversations" ? (total ? `${total} 个会话` : "暂无历史会话") : (requestTotal ? `${requestTotal} 条请求` : "暂无请求记录")}</span>
            </div>
            <button className="playground-icon-button" type="button" onClick={() => setReload((value) => value + 1)} disabled={historyLoading || requestLoading} title="刷新当前列表" aria-label="刷新当前列表"><RefreshCw size={16} aria-hidden="true" /></button>
          </header>
          <div className="playground-history-list">
            {sidebarTab === "conversations" && (
              <>
                {historyLoading && <div className="playground-history-state">正在读取…</div>}
                {!historyLoading && conversations.length === 0 && <div className="playground-history-state">发送第一条消息后，对话会保存在这里。</div>}
                {!historyLoading && conversations.map((conversation) => (
                  <div
                    className={`playground-history-item${conversation.id === conversationId ? " is-active" : ""}`}
                    key={conversation.id}
                  >
                    <button className="playground-history-item-open" type="button" onClick={() => void openConversation(conversation.id)}>
                      <div className="playground-history-item-top"><span className="playground-status-dot is-ok" /><strong>{conversation.title}</strong><span>{conversation.message_count} 条</span></div>
                      <div className="playground-history-item-meta"><span>{conversation.channel} · {conversation.model}</span><span>{new Date(conversation.updated_at * 1000).toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" })}</span></div>
                    </button>
                    {role === "admin" && <button
                      className="playground-history-delete"
                      type="button"
                      disabled={busy || deletingConversationId !== null}
                      onClick={() => void deleteConversation(conversation.id, conversation.title)}
                      title="删除对话"
                      aria-label={`删除对话 ${conversation.title}`}
                    >
                      {deletingConversationId === conversation.id ? <RefreshCw size={14} className="spin" aria-hidden="true" /> : <Trash2 size={14} aria-hidden="true" />}
                    </button>}
                  </div>
                ))}
              </>
            )}
            {sidebarTab === "requests" && (
              <>
                {requestLoading && <div className="playground-history-state">正在读取…</div>}
                {!requestLoading && requestRuns.length === 0 && <div className="playground-history-state">暂无请求记录。</div>}
                {!requestLoading && requestRuns.map((run) => (
                  <div className="playground-history-item" key={run.id}>
                    <div className="playground-history-item-top"><span className={`playground-status-dot is-${run.status}`} /><strong>{run.model}</strong><span>{runStatusLabel(run)}</span></div>
                    <div className="playground-history-item-meta"><span>{run.channel} · HTTP {run.response_status ?? "—"}</span><span>{new Date(run.created_at * 1000).toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" })}</span></div>
                  </div>
                ))}
              </>
            )}
          </div>
          <Pagination
            currentPage={sidebarTab === "conversations" ? page : requestPage}
            pageSize={pageSize}
            total={sidebarTab === "conversations" ? total : requestTotal}
            pageSizes={PAGE_SIZE_OPTIONS}
            layout="prev, pager, next"
            small
            disabled={sidebarTab === "conversations" ? historyLoading : requestLoading}
            onCurrentChange={sidebarTab === "conversations" ? setPage : setRequestPage}
            onSizeChange={(nextPageSize) => {
              if (sidebarTab === "conversations") setPage(1);
              else setRequestPage(1);
              setPageSize(nextPageSize);
            }}
          />
          {response !== null && <details className="playground-latest-response"><summary><span>最新原始响应</span><button type="button" onClick={(event) => { event.preventDefault(); void copyLatestResponse(); }} title="复制最新响应" aria-label="复制最新响应">{copied ? <Check size={14} /> : <Copy size={14} />}</button></summary><pre>{JSON.stringify(response, null, 2)}</pre></details>}
          {modelsError && <div className="playground-history-error">{modelsError}</div>}
        </aside>
      </div>
    </main>
  );
}
