import type { ReactNode, RefObject } from "react";
import { Sparkles } from "lucide-react";
import { PlaygroundTranscript } from "./PlaygroundTranscript";
import type { ChatMessage } from "./types";

type PlaygroundConversationProps = {
  modelLabel: string;
  channelLabel?: string;
  busy: boolean;
  messages: ChatMessage[];
  transcriptRef: RefObject<HTMLDivElement | null>;
  onSuggestion: (prompt: string) => void;
  composer: ReactNode;
};

export function PlaygroundConversation({
  modelLabel,
  channelLabel,
  busy,
  messages,
  transcriptRef,
  onSuggestion,
  composer,
}: PlaygroundConversationProps) {
  return (
    <section className="playground-conversation" aria-label="调试对话">
      <header className="playground-toolbar">
        <div className="playground-context">
          <div className="playground-context-icon" aria-hidden="true"><Sparkles size={17} /></div>
          <div>
            <strong>{modelLabel || "新对话"}</strong>
            <span>{channelLabel ?? "等待选择渠道"} · 流式文本调试</span>
          </div>
        </div>
        <div className={`playground-toolbar-status${busy ? " is-busy" : ""}`}>
          <i aria-hidden="true" />{busy ? "生成中" : modelLabel ? "Ready" : "待配置"}
        </div>
      </header>
      <PlaygroundTranscript messages={messages} busy={busy} transcriptRef={transcriptRef} onSuggestion={onSuggestion} />
      {composer}
    </section>
  );
}
