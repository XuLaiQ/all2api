import { useState } from "react";
import { Check, ChevronDown, Palette, ScanLine, SlidersHorizontal, Sparkles } from "lucide-react";
import { Button } from "../../../app/controls/Button";
import type { ComposerMode } from "./types";

type PlaygroundComposerModeControlsProps = {
  mode: ComposerMode;
};

type PopoverName = "model" | "ratio" | "style" | "template" | "length" | "duration" | null;

const imageModels = [
  ["Seedream 5.0 Flash", "专业快速"],
  ["Seedream 5.0 Pro", "专业出图 · 4 倍消耗"],
  ["Seedream 4.5", "日常生成"],
  ["Seedream 4.0", "基础生图"],
] as const;
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

export function PlaygroundComposerModeControls({ mode }: PlaygroundComposerModeControlsProps) {
  const [open, setOpen] = useState<PopoverName>(null);
  const [imageModel, setImageModel] = useState("Seedream 4.5");
  const [imageRatio, setImageRatio] = useState("自动");
  const [imageStyle, setImageStyle] = useState("人像摄影");
  const [pptLength, setPptLength] = useState("智能推荐");
  const [videoModel, setVideoModel] = useState("Seedance 2.0 Fast");
  const [videoRatio, setVideoRatio] = useState("自动");
  const [videoDuration, setVideoDuration] = useState(10);

  function toggle(name: Exclude<PopoverName, null>) {
    setOpen((current) => current === name ? null : name);
  }

  if (mode === "chat") {
    return <div className="playground-composer-mode-controls"><ControlButton label="豆包" value="快速" icon={<Sparkles size={14} aria-hidden="true" />} onClick={() => undefined} /></div>;
  }

  if (mode === "image") {
    return <div className="playground-composer-mode-controls">
      <div className="playground-setting-popover-wrap"><ControlButton label="模型" value={imageModel} icon={<Sparkles size={14} aria-hidden="true" />} active={open === "model"} onClick={() => toggle("model")} />{open === "model" && <div className="playground-setting-popover playground-model-popover"><span className="playground-setting-title">模型</span><OptionList options={imageModels} selected={imageModel} onSelect={(value) => { setImageModel(value); setOpen(null); }} /></div>}</div>
      <div className="playground-setting-popover-wrap"><ControlButton label="比例" value={imageRatio} icon={<SlidersHorizontal size={14} aria-hidden="true" />} active={open === "ratio"} onClick={() => toggle("ratio")} />{open === "ratio" && <div className="playground-setting-popover playground-ratio-popover"><span className="playground-setting-title">比例</span><div className="playground-ratio-grid">{imageRatios.map((ratio) => <Button key={ratio} variant="unstyled" className={imageRatio === ratio ? "is-selected" : ""} onClick={() => { setImageRatio(ratio); setOpen(null); }}><ScanLine size={16} aria-hidden="true" />{ratio}</Button>)}</div></div>}</div>
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
      <div className="playground-setting-popover-wrap"><ControlButton label="模型" value={videoModel} icon={<Sparkles size={14} aria-hidden="true" />} active={open === "model"} onClick={() => toggle("model")} />{open === "model" && <div className="playground-setting-popover playground-model-popover"><span className="playground-setting-title">模型</span><OptionList options={[["Seedance 2.5", "旗舰视频标杆 · 5 倍消耗"], ["Seedance 2.0", "进阶画面表现 · 2 倍消耗"], ["Seedance 2.0 Fast", "快速出片选择"], ["Seedance 2.0 Mini", "日常生成使用"]]} selected={videoModel} onSelect={(value) => { setVideoModel(value); setOpen(null); }} /></div>}</div>
      <div className="playground-setting-popover-wrap"><ControlButton label="自动" value={`${videoDuration}s`} icon={<SlidersHorizontal size={14} aria-hidden="true" />} active={open === "duration"} onClick={() => toggle("duration")} />{open === "duration" && <div className="playground-setting-popover playground-video-popover"><span className="playground-setting-title">比例</span><div className="playground-ratio-grid">{videoRatios.map((ratio) => <Button key={ratio} variant="unstyled" className={videoRatio === ratio ? "is-selected" : ""} onClick={() => setVideoRatio(ratio)}><ScanLine size={16} aria-hidden="true" />{ratio}</Button>)}</div><span className="playground-setting-title playground-duration-title">时长 <b>{videoDuration}s</b></span><input type="range" min="4" max="15" value={videoDuration} onChange={(event) => setVideoDuration(Number(event.target.value))} /></div>}</div>
    </div>;
  }

  return <div className="playground-composer-mode-controls" />;
}
