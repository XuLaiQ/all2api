import { Check, Copy, MessageCircle, Plus, RefreshCw, Trash2 } from "lucide-react";
import { Button } from "../../../app/controls/Button";
import { Pagination } from "../../../app/data/Pagination";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "../../../app/data/pagination.constants";
import type { PlaygroundConversationSummary, PlaygroundRun } from "../managementApi";
import type { PlaygroundSidebarTab } from "./types";

type PlaygroundHistorySidebarProps = {
  role: "admin" | "viewer";
  sidebarTab: PlaygroundSidebarTab;
  conversations: PlaygroundConversationSummary[];
  requestRuns: PlaygroundRun[];
  conversationId: string | null;
  historyLoading: boolean;
  requestLoading: boolean;
  total: number;
  requestTotal: number;
  page: number;
  requestPage: number;
  pageSize: number;
  busy: boolean;
  deletingConversationId: string | null;
  response: unknown;
  copied: boolean;
  modelsError: string;
  onStartNewConversation: () => void;
  onOpenConversation: (id: string) => void;
  onDeleteConversation: (id: string, title: string) => void;
  onSidebarTabChange: (tab: PlaygroundSidebarTab) => void;
  onReload: () => void;
  onPageChange: (page: number) => void;
  onRequestPageChange: (page: number) => void;
  onPageSizeChange: (pageSize: number) => void;
  onCopyLatestResponse: () => void;
};

function runStatusLabel(run: PlaygroundRun): string {
  if (run.status === "ok" && (run.response_status ?? 500) < 400) return "已完成";
  if (run.status === "running") return "处理中";
  return "失败";
}

function formatUpdatedAt(value: number): string {
  return new Date(value * 1000).toLocaleString("zh-CN", {
    month: "numeric",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function PlaygroundHistorySidebar({
  role,
  sidebarTab,
  conversations,
  requestRuns,
  conversationId,
  historyLoading,
  requestLoading,
  total,
  requestTotal,
  page,
  requestPage,
  pageSize,
  busy,
  deletingConversationId,
  response,
  copied,
  modelsError,
  onStartNewConversation,
  onOpenConversation,
  onDeleteConversation,
  onSidebarTabChange,
  onReload,
  onPageChange,
  onRequestPageChange,
  onPageSizeChange,
  onCopyLatestResponse,
}: PlaygroundHistorySidebarProps) {
  const isConversationView = sidebarTab === "conversations";
  const currentLoading = isConversationView ? historyLoading : requestLoading;
  const currentTotal = isConversationView ? total : requestTotal;

  return (
    <aside className="playground-history" aria-label="调试台侧栏">
      <header className="playground-history-header">
        <div className="playground-history-brand-row">
          <div className="playground-history-brand">
            <div className="playground-history-brand-mark" aria-hidden="true"><MessageCircle size={16} /></div>
            <div><strong>调试对话</strong><span>Conversation space</span></div>
          </div>
          <Button variant="unstyled" className="playground-icon-button playground-history-new" onClick={onStartNewConversation} title="新建对话" aria-label="新建对话">
            <Plus size={17} aria-hidden="true" />
          </Button>
        </div>
        <div className="playground-history-tools">
          <div className="playground-sidebar-tabs" role="tablist" aria-label="调试台侧栏视图">
            <Button variant="unstyled" className={`playground-sidebar-tab${isConversationView ? " is-active" : ""}`} role="tab" aria-selected={isConversationView} onClick={() => onSidebarTabChange("conversations")}>对话历史</Button>
            <Button variant="unstyled" className={`playground-sidebar-tab${!isConversationView ? " is-active" : ""}`} role="tab" aria-selected={!isConversationView} onClick={() => onSidebarTabChange("requests")}>请求记录</Button>
          </div>
          <Button variant="unstyled" className="playground-icon-button" onClick={onReload} disabled={historyLoading || requestLoading} title="刷新当前列表" aria-label="刷新当前列表"><RefreshCw size={15} aria-hidden="true" /></Button>
        </div>
        <span className="playground-history-count">{currentTotal ? `${currentTotal} ${isConversationView ? "个会话" : "条请求"}` : isConversationView ? "暂无历史会话" : "暂无请求记录"}</span>
      </header>
      <div className="playground-history-list">
        {isConversationView && (
          <>
            {historyLoading && <div className="playground-history-state">正在读取…</div>}
            {!historyLoading && conversations.length === 0 && <div className="playground-history-state">发送第一条消息后，对话会保存在这里。</div>}
            {!historyLoading && conversations.map((conversation) => (
              <div className={`playground-history-item${conversation.id === conversationId ? " is-active" : ""}`} key={conversation.id}>
                <Button variant="unstyled" className="playground-history-item-open" onClick={() => onOpenConversation(conversation.id)}>
                  <div className="playground-history-item-top"><span className="playground-status-dot is-ok" /><strong>{conversation.title}</strong><span>{conversation.message_count} 条</span></div>
                  <div className="playground-history-item-meta"><span>{conversation.channel} · {conversation.model}</span><span>{formatUpdatedAt(conversation.updated_at)}</span></div>
                </Button>
                {role === "admin" && <Button variant="unstyled" className="playground-history-delete" disabled={busy || deletingConversationId !== null} onClick={() => onDeleteConversation(conversation.id, conversation.title)} title="删除对话" aria-label={`删除对话 ${conversation.title}`}>
                  {deletingConversationId === conversation.id ? <RefreshCw size={14} className="spin" aria-hidden="true" /> : <Trash2 size={14} aria-hidden="true" />}
                </Button>}
              </div>
            ))}
          </>
        )}
        {!isConversationView && (
          <>
            {requestLoading && <div className="playground-history-state">正在读取…</div>}
            {!requestLoading && requestRuns.length === 0 && <div className="playground-history-state">暂无请求记录。</div>}
            {!requestLoading && requestRuns.map((run) => (
              <div className="playground-history-item" key={run.id}>
                <div className="playground-history-item-top"><span className={`playground-status-dot is-${run.status}`} /><strong>{run.model}</strong><span>{runStatusLabel(run)}</span></div>
                <div className="playground-history-item-meta"><span>{run.channel} · HTTP {run.response_status ?? "—"}</span><span>{formatUpdatedAt(run.created_at)}</span></div>
              </div>
            ))}
          </>
        )}
      </div>
      <Pagination
        currentPage={isConversationView ? page : requestPage}
        pageSize={pageSize || DEFAULT_PAGE_SIZE}
        total={currentTotal}
        pageSizes={PAGE_SIZE_OPTIONS}
        layout="prev, pager, next"
        small
        disabled={currentLoading}
        onCurrentChange={isConversationView ? onPageChange : onRequestPageChange}
        onSizeChange={onPageSizeChange}
      />
      {response !== null && <details className="playground-latest-response"><summary><span>最新原始响应</span><Button variant="unstyled" onClick={(event) => { event.preventDefault(); onCopyLatestResponse(); }} title="复制最新响应" aria-label="复制最新响应">{copied ? <Check size={14} /> : <Copy size={14} />}</Button></summary><pre>{JSON.stringify(response, null, 2)}</pre></details>}
      {modelsError && <div className="playground-history-error">{modelsError}</div>}
    </aside>
  );
}
