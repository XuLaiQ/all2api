import type { RefObject } from "react";
import { AlertCircle, Bot, FileJson, UserRound } from "lucide-react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Button } from "../../../app/controls/Button";
import { chatSuggestionOptions, type ChatMessage } from "./types";

type PlaygroundTranscriptProps = {
  messages: ChatMessage[];
  busy: boolean;
  transcriptRef: RefObject<HTMLDivElement | null>;
  onSuggestion: (prompt: string) => void;
};

type MediaItem = { kind: "image" | "video"; src: string; label: string };

function mediaItems(value: unknown): MediaItem[] {
  if (!value || typeof value !== "object") return [];
  const data = (value as { data?: unknown }).data;
  if (!Array.isArray(data)) return [];
  return data.flatMap<MediaItem>((item, index) => {
    if (!item || typeof item !== "object") return [];
    const entry = item as { url?: unknown; video_url?: unknown; b64_json?: unknown };
    const video = typeof entry.video_url === "string" ? entry.video_url : "";
    const image = typeof entry.url === "string"
      ? entry.url
      : typeof entry.b64_json === "string" ? `data:image/png;base64,${entry.b64_json}` : "";
    if (video) return [{ kind: "video", src: video, label: `生成视频 ${index + 1}` }];
    if (image) return [{ kind: "image", src: image, label: `生成图片 ${index + 1}` }];
    return [];
  });
}

export function PlaygroundTranscript({ messages, busy, transcriptRef, onSuggestion }: PlaygroundTranscriptProps) {
  return (
    <div className="playground-transcript" ref={transcriptRef} aria-live="polite">
      {messages.length === 0 ? (
        <div className="playground-empty-state">
          <div className="playground-empty-icon" aria-hidden="true"><Bot size={24} /></div>
          <h2>有什么我能帮你的吗？</h2>
          <p>选择渠道和模型，开始一段新的调试对话。</p>
          <div className="playground-suggestion-label">为你推荐</div>
          <div className="playground-suggestion-list">
            {chatSuggestionOptions.map((item) => (
              <Button key={item.label} variant="unstyled" className="playground-suggestion" onClick={() => onSuggestion(item.prompt)}>
                {item.label}
              </Button>
            ))}
          </div>
        </div>
      ) : messages.map((item) => (
        <article className={`playground-message is-${item.role}`} key={item.id}>
          <div className="playground-message-avatar" aria-hidden="true">
            {item.role === "user" ? <UserRound size={16} /> : item.role === "error" ? <AlertCircle size={16} /> : <Bot size={17} />}
          </div>
          <div className="playground-message-content">
            <div className="playground-message-name">{item.role === "user" ? "你" : item.role === "error" ? "调试台" : item.model || "助手"}</div>
            <div className="playground-message-text">
              {item.role === "assistant"
                ? item.content
                  ? <ReactMarkdown remarkPlugins={[remarkGfm]}>{item.content}</ReactMarkdown>
                  : busy ? <span className="playground-stream-cursor" aria-hidden="true" /> : "（空响应）"
                : item.content}
            </div>
            {item.raw !== undefined && mediaItems(item.raw).length > 0 && <div className="playground-media-results">
              {mediaItems(item.raw).map((media) => media.kind === "video"
                ? <video key={media.src} className="playground-media-video" src={media.src} controls preload="metadata" aria-label={media.label} />
                : <a key={media.src} href={media.src} target="_blank" rel="noreferrer"><img className="playground-media-image" src={media.src} alt={media.label} /></a>)}
            </div>}
            {item.raw !== undefined && <details className="playground-raw-message"><summary><FileJson size={14} aria-hidden="true" />查看原始响应</summary><pre>{JSON.stringify(item.raw, null, 2)}</pre></details>}
          </div>
        </article>
      ))}
      {busy && <div className="playground-typing" role="status"><span /><span /><span />正在生成回复…</div>}
    </div>
  );
}
