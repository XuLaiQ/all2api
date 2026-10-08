import { useState } from "react";
import { Check, ChevronDown, Palette, ScanLine, SlidersHorizontal, Sparkles } from "lucide-react";
import { Button } from "../../../app/controls/Button";
import type { ModelRecord } from "../../models/modelsApi";
import type { ComposerMode, PlaygroundGenerationSettings } from "./types";

type PlaygroundComposerModeControlsProps = {
  mode: ComposerMode;
  availableModels: ModelRecord[];
  selectedModel?: ModelRecord;
  settings: PlaygroundGenerationSettings;
  onModelChange: (model: string) => void;
  onSettingsChange: (settings: PlaygroundGenerationSettings) => void;
};

type PopoverName = "model" | "ratio" | "style" | "template" | "length" | "duration" | null;

const imageRatios = ["自动", "9:16", "2:3", "3:4", "1:1", "4:3", "3:2", "16:9"];
const imageStyles = ["人像摄影", "电影写真", "中国风", "动漫", "3D渲染", "赛博朋克", "CG 动画", "水墨画", "油画", "古典", "水彩画", "卡通"];
const pptLengths = ["智能推荐", "精简", "适中", "详细"];
const videoRatios = ["自动", "3:4", "4:3", "9:16", "16:9", "1:1", "21:9"];

function ControlButton({ label, value, icon, active, onClick }: { label: string; value?: string; icon?: React.ReactNode; active?: boolean; onClick: () => void }) {
  return <Button variant="unstyled" className={`playground-setting-control${active ? " is-active" : ""}`} onClick={onClick} aria-expanded={active}>
    {icon}{label}{value ? <> {value}</> : null}<ChevronDown size={13} aria-hidden="true" className={active ? "is-up" : undefined} />
  </Button>;
}

function OptionList({ options, selected, onSelect, className = "" }: { options: readonly (readonly [string, string])[]; selected: string; onSelect: (value: string) => void; className?: string }) {
  return <div className={`playground-setting-list${className ? ` ${className}` : ""}`}>
    {options.map(([value, hint]) => <Button key={value} variant="unstyled" className={`playground-setting-option${selected === value ? " is-selected" : ""}`} onClick={() => onSelect(value)}>
      <span><strong>{value}</strong>{hint && <small>{hint}</small>}</span>{selected === value && <Check size={15} aria-hidden="true" />}
    </Button>)}
  </div>;
}

export function PlaygroundComposerModeControls({
  mode,
  availableModels,
  selectedModel,
  settings,
  onModelChange,
  onSettingsChange,
}: PlaygroundComposerModeControlsProps) {
  const [open, setOpen] = useState<PopoverName>(null);
  const [imageStyle, setImageStyle] = useState("人像摄影");
  const [pptLength, setPptLength] = useState("智能推荐");
  const modelOptions = availableModels.map((item) => [
    item.upstream_id,
    item.display_name === item.upstream_id ? "" : item.display_name,
  ] as const);
  const selectedModelValue = selectedModel?.upstream_id ?? availableModels[0]?.upstream_id ?? "";
  const modelLabel = selectedModel?.display_name ?? (selectedModelValue || "未选择");

  function toggle(name: Exclude<PopoverName, null>) {
    setOpen((current) => current === name ? null : name);
  }

  if (mode === "chat") {
    return <div className="playground-composer-mode-controls"><ControlButton label="模型" value={modelLabel} icon={<Sparkles size={14} aria-hidden="true" />} onClick={() => undefined} /></div>;
  }

  if (mode === "image") {
    return <div className="playground-composer-mode-controls">
      <div className="playground-setting-popover-wrap"><ControlButton label="模型" value={modelLabel} icon={<Sparkles size={14} aria-hidden="true" />} active={open === "model"} onClick={() => toggle("model")} />{open === "model" && <div className="playground-setting-popover playground-model-popover"><span className="playground-setting-title">模型</span><OptionList options={modelOptions} selected={selectedModelValue} onSelect={(value) => { onModelChange(value); setOpen(null); }} /></div>}</div>
      <div className="playground-setting-popover-wrap"><ControlButton label="比例" value={settings.imageRatio} icon={<SlidersHorizontal size={14} aria-hidden="true" />} active={open === "ratio"} onClick={() => toggle("ratio")} />{open === "ratio" && <div className="playground-setting-popover playground-ratio-popover"><span className="playground-setting-title">比例</span><div className="playground-ratio-grid">{imageRatios.map((ratio) => <Button key={ratio} variant="unstyled" className={settings.imageRatio === ratio ? "is-selected" : ""} onClick={() => { onSettingsChange({ ...settings, imageRatio: ratio }); setOpen(null); }}><ScanLine size={16} aria-hidden="true" />{ratio}</Button>)}</div></div>}</div>
      <div className="playground-setting-popover-wrap"><ControlButton label="风格" value={imageStyle} icon={<Palette size={14} aria-hidden="true" />} active={open === "style"} onClick={() => toggle("style")} />{open === "style" && <div className="playground-setting-popover playground-style-popover"><span className="playground-setting-title">风格</span><div className="playground-style-list">{imageStyles.map((style, index) => <Button key={style} variant="unstyled" className={imageStyle === style ? "is-selected" : ""} onClick={() => { setImageStyle(style); setOpen(null); }}><i className={`playground-style-swatch style-${index}`} aria-hidden="true" />{style}</Button>)}</div></div>}</div>
      <div className="playground-setting-popover-wrap"><ControlButton label="模板" icon={<ScanLine size={14} aria-hidden="true" />} active={open === "template"} onClick={() => toggle("template")} />{open === "template" && <div className="playground-setting-popover playground-template-popover"><span className="playground-setting-title">模板</span><Button variant="unstyled" className="playground-setting-option is-selected"><span><strong>智能匹配</strong><small>根据描述自动选择</small></span><Check size={15} aria-hidden="true" /></Button><Button variant="unstyled" className="playground-setting-option"><span><strong>自定义模板</strong><small>使用上传的参考图</small></span></Button></div>}</div>
    </div>;
  }

  if (mode === "ppt") {
    return <div className="playground-composer-mode-controls">
      <div className="playground-setting-popover-wrap"><ControlButton label="篇幅" value={pptLength} icon={<ScanLine size={14} aria-hidden="true" />} active={open === "length"} onClick={() => toggle("length")} />{open === "length" && <div className="playground-setting-popover playground-length-popover"><span className="playground-setting-title">篇幅</span><OptionList options={pptLengths.map((item) => [item, item === "智能推荐" ? "根据内容自动决定" : ""] as const)} selected={pptLength} onSelect={(value) => { setPptLength(value); setOpen(null); }} /></div>}</div>
      <ControlButton label="风格" value="智能匹配" icon={<Palette size={14} aria-hidden="true" />} onClick={() => undefined} />
    </div>;
  }

  if (mode === "video") {
    return <div className="playground-composer-mode-controls">
      <div className="playground-setting-popover-wrap"><ControlButton label="模型" value={modelLabel} icon={<Sparkles size={14} aria-hidden="true" />} active={open === "model"} onClick={() => toggle("model")} />{open === "model" && <div className="playground-setting-popover playground-model-popover"><span className="playground-setting-title">模型</span><OptionList options={modelOptions} selected={selectedModelValue} onSelect={(value) => { onModelChange(value); setOpen(null); }} /></div>}</div>
      <div className="playground-setting-popover-wrap"><ControlButton label="自动" value={`${settings.videoDuration}s`} icon={<SlidersHorizontal size={14} aria-hidden="true" />} active={open === "duration"} onClick={() => toggle("duration")} />{open === "duration" && <div className="playground-setting-popover playground-video-popover"><span className="playground-setting-title">比例</span><div className="playground-ratio-grid">{videoRatios.map((ratio) => <Button key={ratio} variant="unstyled" className={settings.videoRatio === ratio ? "is-selected" : ""} onClick={() => { onSettingsChange({ ...settings, videoRatio: ratio }); setOpen(null); }}><ScanLine size={16} aria-hidden="true" />{ratio}</Button>)}</div><span className="playground-setting-title playground-duration-title">时长 <b>{settings.videoDuration}s</b></span><input type="range" min="4" max="15" value={settings.videoDuration} onChange={(event) => onSettingsChange({ ...settings, videoDuration: Number(event.target.value) })} /></div>}</div>
    </div>;
  }

  return <div className="playground-composer-mode-controls" />;
}
