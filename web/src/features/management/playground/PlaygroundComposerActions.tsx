import { useState } from "react";
import { CircleHelp, Image as ImageIcon, Layers3, Mic, MoreHorizontal, PenLine, Presentation, Search, Video } from "lucide-react";
import { Button } from "../../../app/controls/Button";
import type { ComposerMode } from "./types";

type PlaygroundComposerActionsProps = {
  mode: ComposerMode;
  onModeChange: (mode: ComposerMode) => void;
  onAttach: () => void;
};

const modeLabels: Record<Exclude<ComposerMode, "chat">, string> = {
  image: "图像生成",
  ppt: "PPT 生成",
  video: "视频生成",
  writing: "帮我写作",
  quiz: "解题答疑",
  transcribe: "录音转写",
  search: "联网搜索",
  psd: "PSD 生成",
};

export function PlaygroundComposerActions({ mode, onModeChange, onAttach }: PlaygroundComposerActionsProps) {
  const [moreOpen, setMoreOpen] = useState(false);

  function choose(nextMode: ComposerMode) {
    onModeChange(nextMode);
    setMoreOpen(false);
  }

  const selectedMode = mode === "chat" ? null : mode;
  if (selectedMode) {
    return (
      <div className="playground-composer-actions">
        <Button variant="unstyled" className="playground-composer-action playground-composer-attach" onClick={onAttach} title="添加参考图片" aria-label="添加参考图片"><span aria-hidden="true">+</span></Button>
        <span className="playground-composer-action-divider" aria-hidden="true" />
        <Button variant="unstyled" className="playground-composer-action is-active playground-composer-selected-mode" onClick={() => choose("chat")} title="退出当前模式">
          {selectedMode === "image" && <ImageIcon size={15} aria-hidden="true" />}
          {selectedMode === "ppt" && <Presentation size={15} aria-hidden="true" />}
          {selectedMode === "video" && <Video size={15} aria-hidden="true" />}
          {selectedMode === "writing" && <PenLine size={15} aria-hidden="true" />}
          {selectedMode === "quiz" && <CircleHelp size={15} aria-hidden="true" />}
          {selectedMode === "transcribe" && <Mic size={15} aria-hidden="true" />}
          {selectedMode === "search" && <Search size={15} aria-hidden="true" />}
          {selectedMode === "psd" && <Layers3 size={15} aria-hidden="true" />}
          {modeLabels[selectedMode]} <span aria-hidden="true">×</span>
        </Button>
      </div>
    );
  }

  return (
    <div className="playground-composer-actions">
      <Button variant="unstyled" className="playground-composer-action playground-composer-attach" onClick={onAttach} title="添加参考图片" aria-label="添加参考图片"><span aria-hidden="true">+</span></Button>
      <span className="playground-composer-action-divider" aria-hidden="true" />
      <Button variant="unstyled" className="playground-composer-action is-active" onClick={() => choose("chat")}><span className="playground-composer-action-bubble" aria-hidden="true" />对话</Button>
      <Button variant="unstyled" className="playground-composer-action" onClick={() => choose("image")}><ImageIcon size={15} aria-hidden="true" />图像生成</Button>
      <Button variant="unstyled" className="playground-composer-action" onClick={() => choose("ppt")}><Presentation size={15} aria-hidden="true" />PPT 生成</Button>
      <Button variant="unstyled" className="playground-composer-action" onClick={() => choose("video")}><Video size={15} aria-hidden="true" />视频生成</Button>
      <Button variant="unstyled" className="playground-composer-action" onClick={() => choose("writing")}><PenLine size={15} aria-hidden="true" />帮我写作</Button>
      <Button variant="unstyled" className="playground-composer-action" onClick={() => choose("quiz")}><CircleHelp size={15} aria-hidden="true" />解题答疑</Button>
      <Button variant="unstyled" className="playground-composer-action" onClick={() => choose("transcribe")}><Mic size={15} aria-hidden="true" />录音转写</Button>
      <div className="playground-composer-more">
        <Button variant="unstyled" className={`playground-composer-action${moreOpen ? " is-active" : ""}`} onClick={() => setMoreOpen((current) => !current)} aria-expanded={moreOpen} aria-haspopup="menu"><MoreHorizontal size={16} aria-hidden="true" />更多</Button>
        {moreOpen && <div className="playground-more-menu" role="menu">
          <Button variant="unstyled" role="menuitem" onClick={() => choose("search")}><Search size={14} aria-hidden="true" />联网搜索</Button>
          <Button variant="unstyled" role="menuitem" onClick={() => choose("psd")}><Layers3 size={14} aria-hidden="true" />PSD 生成</Button>
        </div>}
      </div>
    </div>
  );
}
