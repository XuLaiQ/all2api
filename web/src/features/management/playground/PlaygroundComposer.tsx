import { useRef, useState, type ChangeEvent, type FormEvent, type KeyboardEvent } from "react";
import { Send, X } from "lucide-react";
import { Button } from "../../../app/controls/Button";
import { Select } from "../../../app/controls/Select";
import type { ModelRecord } from "../../models/modelsApi";
import type { ChannelOverview } from "../../usage/usageApi";
import { PlaygroundComposerActions } from "./PlaygroundComposerActions";
import { PlaygroundComposerModeControls } from "./PlaygroundComposerModeControls";
import type { ComposerMode } from "./types";

export type ComposerImage = {
  id: string;
  name: string;
  url: string;
};

type PlaygroundComposerProps = {
  role: "admin" | "viewer";
  channels: ChannelOverview[];
  channel: string;
  model: string;
  selectedChannel?: ChannelOverview;
  selectedModel?: ModelRecord;
  availableModels: ModelRecord[];
  modelsLoading: boolean;
  message: string;
  busy: boolean;
  mode: ComposerMode;
  onChannelChange: (channel: string) => void;
  onModelChange: (model: string) => void;
  onMessageChange: (event: ChangeEvent<HTMLTextAreaElement>) => void;
  onKeyDown: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
  onModeChange: (mode: ComposerMode) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>, images: string[]) => void;
};

function readImage(file: File): Promise<ComposerImage> {
  return new Promise((resolve, reject) => {
    if (!file.type.startsWith("image/")) {
      reject(new Error(`${file.name} 不是图片文件`));
      return;
    }
    if (file.size > 10 * 1024 * 1024) {
      reject(new Error(`${file.name} 超过 10MB`));
      return;
    }
    const reader = new FileReader();
    reader.onload = () => resolve({
      id: `${file.name}-${file.size}-${file.lastModified}-${Math.random().toString(16).slice(2)}`,
      name: file.name,
      url: String(reader.result || ""),
    });
    reader.onerror = () => reject(reader.error ?? new Error(`${file.name} 读取失败`));
    reader.readAsDataURL(file);
  });
}

export function PlaygroundComposer({
  role,
  channels,
  channel,
  model,
  selectedChannel,
  selectedModel,
  availableModels,
  modelsLoading,
  message,
  busy,
  mode,
  onChannelChange,
  onModelChange,
  onMessageChange,
  onKeyDown,
  onModeChange,
  onSubmit,
}: PlaygroundComposerProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [images, setImages] = useState<ComposerImage[]>([]);
  const [uploadError, setUploadError] = useState("");

  async function appendFiles(files: FileList | null) {
    if (!files?.length) return;
    setUploadError("");
    try {
      const values = await Promise.all(Array.from(files).map(readImage));
      setImages((current) => [...current, ...values].slice(0, 4));
    } catch (cause: unknown) {
      setUploadError(cause instanceof Error ? cause.message : "图片读取失败");
    }
  }

  const placeholder = mode === "search"
    ? "输入要搜索的问题…"
    : mode === "ppt"
      ? "描述要生成的 PPT…"
      : mode === "psd"
        ? "描述要生成的 PSD…"
        : role === "admin" ? "描述你的问题或任务…" : "当前角色仅可查看历史运行记录";
  const sendDisabled = role !== "admin"
    || busy
    || !channel
    || !selectedModel
    || !message.trim()
    || (mode === "psd" && images.length === 0);

  return (
    <form className="playground-composer" onSubmit={(event) => onSubmit(event, images.map((image) => image.url))}>
      <div className="playground-composer-box">
        <div className="playground-composer-tools">
          <label className="playground-composer-chip"><span>渠道</span><Select
            value={channel}
            onChange={onChannelChange}
            disabled={role !== "admin"}
            placeholder="选择渠道"
            ariaLabel="选择渠道"
            options={channels.map((item) => ({ value: item.slug, label: `${item.name} (${item.slug})` }))}
          /></label>
          <label className="playground-composer-chip"><span>模型</span><Select
            value={model}
            onChange={onModelChange}
            disabled={role !== "admin" || !channel || modelsLoading || availableModels.length === 0}
            placeholder={modelsLoading ? "读取模型…" : !channel ? "先选渠道" : "选择模型"}
            ariaLabel="选择模型"
            options={availableModels.map((item) => ({
              value: item.upstream_id,
              label: item.display_name === item.upstream_id ? item.display_name : `${item.display_name} (${item.upstream_id})`,
            }))}
          /></label>
          <PlaygroundComposerActions mode={mode} onModeChange={onModeChange} onAttach={() => inputRef.current?.click()} />
          <PlaygroundComposerModeControls mode={mode} />
          <input ref={inputRef} className="playground-composer-file-input" type="file" accept="image/png,image/jpeg,image/webp,image/gif" multiple onChange={(event) => { void appendFiles(event.target.files); event.currentTarget.value = ""; }} disabled={role !== "admin" || busy} />
        </div>
        {images.length > 0 && <div className="playground-composer-attachments">{images.map((image) => <div key={image.id} className="playground-composer-attachment"><img src={image.url} alt={image.name} /><Button variant="unstyled" onClick={() => setImages((current) => current.filter((item) => item.id !== image.id))} title={`移除 ${image.name}`} aria-label={`移除 ${image.name}`}><X size={12} /></Button></div>)}</div>}
        {uploadError && <div className="playground-composer-upload-error" role="alert">{uploadError}</div>}
        <div className="playground-composer-row">
          <textarea value={message} onChange={onMessageChange} onKeyDown={onKeyDown} rows={2} placeholder={placeholder} disabled={role !== "admin" || !selectedModel} aria-label="消息" />
          <Button variant="unstyled" className="playground-send-button" type="submit" disabled={sendDisabled} title={mode === "chat" ? "发送消息" : "开始生成"} aria-label={mode === "chat" ? "发送消息" : "开始生成"}>
            <Send size={17} aria-hidden="true" />
          </Button>
        </div>
      </div>
      <div className="playground-composer-footer"><span>Enter 发送 · Shift + Enter 换行</span><span>{selectedModel ? `${selectedChannel?.name ?? channel} / ${model}` : "请选择可用模型"}</span></div>
    </form>
  );
}
