import { useState } from "react";
import { Check, Copy, Download, ExternalLink, FileArchive, FileText, ImagePlus, LoaderCircle, Search, X } from "lucide-react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { runPlaygroundFileTask, runPlaygroundSearch, type PlaygroundFileResult, type PlaygroundSearchResult } from "./managementApi";

type PlaygroundCapabilityProps = {
  channel: string;
  model: string;
};

type SelectedImage = {
  id: string;
  name: string;
  url: string;
};

function readImage(file: File): Promise<SelectedImage> {
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
    reader.onload = () => {
      const url = String(reader.result || "");
      if (!url.startsWith("data:image/")) {
        reject(new Error(`${file.name} 读取失败`));
        return;
      }
      resolve({ id: `${file.name}-${file.size}-${file.lastModified}-${Math.random().toString(16).slice(2)}`, name: file.name, url });
    };
    reader.onerror = () => reject(reader.error ?? new Error(`${file.name} 读取失败`));
    reader.readAsDataURL(file);
  });
}

function errorText(cause: unknown): string {
  return cause instanceof ApiClientError ? cause.message : cause instanceof Error ? cause.message : "调试请求失败";
}

export function PlaygroundSkillPanel() {
  const [copied, setCopied] = useState(false);
  const apiBase = typeof window === "undefined" ? "/v1" : `${window.location.origin}/v1`;
  const skill = `---
name: all2api-search
description: 通过当前 All2API 的联网搜索接口回答需要最新信息的问题。
---

# All2API Search

当用户需要联网搜索、核实事实或查询最新信息时，调用：

POST ${apiBase}/search
Authorization: Bearer <API_KEY>
Content-Type: application/json

{"prompt":"<search question>"}

返回 answer 和 sources，并保留来源链接。`;

  async function copySkill() {
    await navigator.clipboard.writeText(skill);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1600);
  }

  function downloadSkill() {
    const blob = new Blob([skill], { type: "text/markdown;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = "all2api-search.SKILL.md";
    anchor.click();
    URL.revokeObjectURL(url);
  }

  return (
    <section className="playground-tool-panel">
      <div className="playground-tool-heading"><div><span className="page-eyebrow">SKILL</span><h2>联网搜索 Skill</h2><p>生成可直接交给代理使用的本地搜索 Skill 文件。</p></div><div className="playground-tool-actions"><Button variant="secondary" onClick={() => void copySkill()}>{copied ? <Check size={15} /> : <Copy size={15} />}复制</Button><Button variant="secondary" onClick={downloadSkill}><Download size={15} />下载</Button></div></div>
      <pre className="playground-code-block">{skill}</pre>
    </section>
  );
}

export function PlaygroundSearchPanel({ channel, model }: PlaygroundCapabilityProps) {
  const [prompt, setPrompt] = useState("帮我搜索 ChatGPT 最新动态");
  const [result, setResult] = useState<PlaygroundSearchResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function submit() {
    const value = prompt.trim();
    if (!value || busy) return;
    setBusy(true);
    setError("");
    try {
      setResult(await runPlaygroundSearch({ channel: channel || "chatgpt", model: model || "auto", prompt: value }));
    } catch (cause: unknown) {
      setResult(null);
      setError(errorText(cause));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="playground-tool-panel playground-search-panel">
      <div className="playground-tool-heading"><div><span className="page-eyebrow">SEARCH</span><h2>联网搜索</h2><p>使用当前 ChatGPT 账号执行搜索并保留来源链接。</p></div></div>
      <div className="playground-tool-form"><textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} rows={4} placeholder="输入搜索问题" disabled={busy} /><Button variant="primary" className="primary-action-button" onClick={() => void submit()} disabled={busy || !prompt.trim()}>{busy ? <LoaderCircle size={16} className="spin" /> : <Search size={16} />}开始搜索</Button></div>
      {error && <div className="notice notice-error" role="alert">{error}</div>}
      {result && <div className="playground-search-result"><ReactMarkdown remarkPlugins={[remarkGfm]}>{result.answer || "（空响应）"}</ReactMarkdown>{result.sources.length > 0 && <div className="playground-source-list"><strong>来源</strong>{result.sources.map((source) => <a key={source.url} href={source.url} target="_blank" rel="noreferrer"><ExternalLink size={14} />{source.title || source.url}</a>)}</div>}</div>}
    </section>
  );
}

export function PlaygroundEditableFilePanel({ kind, channel, model }: PlaygroundCapabilityProps & { kind: "ppt" | "psd" }) {
  const [prompt, setPrompt] = useState(kind === "ppt" ? "制作一份 8 页以内的商务科技风季度运营汇报 PPT。" : "按原图位置拆分海报元素并输出可编辑 PSD 和图层素材包。");
  const [images, setImages] = useState<SelectedImage[]>([]);
  const [result, setResult] = useState<PlaygroundFileResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function appendFiles(files: FileList | null) {
    if (!files?.length) return;
    setError("");
    try {
      const values = await Promise.all(Array.from(files).map(readImage));
      setImages((current) => [...current, ...values].slice(0, 4));
    } catch (cause: unknown) {
      setError(errorText(cause));
    }
  }

  async function submit() {
    if (!prompt.trim() || busy || (kind === "psd" && images.length === 0)) return;
    setBusy(true);
    setError("");
    setResult(null);
    try {
      const next = await runPlaygroundFileTask({
        channel: channel || "chatgpt",
        model: model || "auto",
        kind,
        prompt: prompt.trim(),
        base64_images: images.map((image) => image.url),
      });
      setResult(next);
      if (next.status === "error") setError(next.error || "文件任务失败");
    } catch (cause: unknown) {
      setError(errorText(cause));
    } finally {
      setBusy(false);
    }
  }

  const title = kind === "ppt" ? "PPT 生成" : "PSD 生成";
  return (
    <section className="playground-tool-panel playground-file-panel">
      <div className="playground-tool-heading"><div><span className="page-eyebrow">{kind.toUpperCase()}</span><h2>{title}</h2><p>{kind === "ppt" ? "提交需求后生成可编辑演示文稿和素材包。" : "上传海报后生成分层 PSD 和图层素材包。"}</p></div></div>
      <div className="playground-tool-form"><textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} rows={5} placeholder="输入生成需求" disabled={busy} />{(kind === "ppt" || kind === "psd") && <label className="playground-upload-zone"><ImagePlus size={17} /><span>{kind === "psd" ? "选择原图" : "添加参考图"}<small>单张不超过 10MB，最多 4 张</small></span><input type="file" accept="image/png,image/jpeg,image/webp,image/gif" multiple onChange={(event) => { void appendFiles(event.target.files); event.currentTarget.value = ""; }} disabled={busy} /></label>}<Button variant="primary" className="primary-action-button" onClick={() => void submit()} disabled={busy || !prompt.trim() || (kind === "psd" && images.length === 0)}>{busy ? <LoaderCircle size={16} className="spin" /> : <FileText size={16} />}{busy ? "生成中" : "开始生成"}</Button></div>
      {images.length > 0 && <div className="playground-image-strip">{images.map((image) => <div key={image.id}><img src={image.url} alt={image.name} /><Button variant="unstyled" onClick={() => setImages((current) => current.filter((item) => item.id !== image.id))} title={`移除 ${image.name}`} aria-label={`移除 ${image.name}`}><X size={14} /></Button></div>)}</div>}
      {error && <div className="notice notice-error" role="alert">{error}</div>}
      {result?.status === "success" && <div className="playground-file-result"><strong>文件已生成</strong>{result.primary_url && <a href={result.primary_url} target="_blank" rel="noreferrer"><FileText size={15} />{kind === "ppt" ? "下载 PPT" : "下载 PSD"}</a>}{result.zip_url && <a href={result.zip_url} target="_blank" rel="noreferrer"><FileArchive size={15} />下载素材包</a>}</div>}
    </section>
  );
}
