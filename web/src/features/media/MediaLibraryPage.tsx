import { useEffect, useState } from "react";
import { Archive, Download, Eraser, FileImage, FileText, Image as ImageIcon, Play, RefreshCw, Trash2, Video } from "lucide-react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { Select } from "../../app/controls/Select";
import { Pagination } from "../../app/data/Pagination";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "../../app/data/pagination.constants";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import { deleteMediaAsset, fetchMediaAssets, saveWatermarkRemovedAsset, type MediaAsset, type MediaAssetFilters, type MediaKind } from "./mediaApi";
import { removeGeneratedWatermark } from "./watermark";
import "./MediaLibraryPage.css";

const kindLabels: Record<MediaKind, string> = {
  image: "图片",
  video: "视频",
  ppt: "PPT",
  psd: "PSD",
  archive: "素材包",
  other: "文件",
};

function formatBytes(value: number): string {
  if (!value) return "远程资源";
  if (value < 1024 * 1024) return `${Math.max(1, Math.round(value / 1024))} KB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
}

function formatDate(value: number): string {
  return new Date(value * 1000).toLocaleString("zh-CN", { dateStyle: "medium", timeStyle: "short" });
}

function AssetIcon({ kind }: { kind: MediaKind }) {
  if (kind === "image") return <ImageIcon size={24} aria-hidden="true" />;
  if (kind === "video") return <Video size={24} aria-hidden="true" />;
  if (kind === "archive") return <Archive size={24} aria-hidden="true" />;
  if (kind === "psd") return <FileImage size={24} aria-hidden="true" />;
  return <FileText size={24} aria-hidden="true" />;
}

export function MediaLibraryPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManage = role === "admin";
  const [assets, setAssets] = useState<MediaAsset[]>([]);
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [filters, setFilters] = useState<MediaAssetFilters>({});
  const [draftSearch, setDraftSearch] = useState("");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  const [selected, setSelected] = useState<MediaAsset | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [watermarking, setWatermarking] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    fetchChannels(controller.signal).then(setChannels).catch(() => undefined);
    return () => controller.abort();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    fetchMediaAssets(page, pageSize, filters, controller.signal)
      .then((result) => {
        setAssets(result.data);
        setTotal(result.pagination.total);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) setError(cause instanceof ApiClientError ? cause.message : "读取素材库失败");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, pageSize, filters, reload]);

  function applySearch() {
    setPage(1);
    setFilters((current) => ({ ...current, search: draftSearch.trim() || undefined }));
  }

  async function remove(asset: MediaAsset) {
    if (!canManage || deleting) return;
    if (!window.confirm(`确定删除“${asset.filename}”吗？`)) return;
    setDeleting(asset.id);
    try {
      await deleteMediaAsset(asset.id);
      if (selected?.id === asset.id) setSelected(null);
      setReload((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "删除素材失败");
    } finally {
      setDeleting(null);
    }
  }

  async function removeWatermark(asset: MediaAsset) {
    if (!canManage || asset.kind !== "image" || watermarking) return;
    setWatermarking(asset.id);
    setError("");
    try {
      const result = await removeGeneratedWatermark(asset.content_url);
      const stem = asset.filename.replace(/\.[^.]+$/, "") || "image";
      const saved = await saveWatermarkRemovedAsset(asset.id, result.dataUrl, `${stem}_clean.png`);
      setAssets((current) => [saved, ...current.filter((item) => item.id !== saved.id)]);
      setTotal((current) => current + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : cause instanceof Error ? cause.message : "去水印失败");
    } finally {
      setWatermarking(null);
    }
  }

  return (
    <main className="page-content data-page media-library-page">
      <div className="page-heading">
        <div>
          <span className="page-eyebrow">MEDIA LIBRARY</span>
          <h1>素材库</h1>
          <p>{total.toLocaleString("zh-CN")} 个生成素材 · 图片、视频、PPT、PSD 和素材包统一保存</p>
        </div>
        <Button variant="secondary" onClick={() => setReload((value) => value + 1)} disabled={loading} title="刷新素材库">
          <RefreshCw size={15} aria-hidden="true" className={loading ? "spin" : undefined} />刷新
        </Button>
      </div>

      <div className="media-filter-form">
        <label><span>类型</span><Select
          value={filters.kind ?? ""}
          onChange={(value) => { setPage(1); setFilters((current) => ({ ...current, kind: (value || undefined) as MediaKind | undefined })); }}
          options={[{ value: "", label: "全部类型" }, ...Object.entries(kindLabels).map(([value, label]) => ({ value, label }))]}
        /></label>
        <label><span>渠道</span><Select
          value={filters.channel ?? ""}
          onChange={(channel) => { setPage(1); setFilters((current) => ({ ...current, channel: channel || undefined })); }}
          options={[{ value: "", label: "全部渠道" }, ...channels.map((channel) => ({ value: channel.slug, label: channel.name }))]}
        /></label>
        <label className="media-search-field"><span>搜索</span><input value={draftSearch} onChange={(event) => setDraftSearch(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter") applySearch(); }} placeholder="文件名、模型或渠道" /></label>
        <Button variant="primary" onClick={applySearch}>筛选</Button>
        <Button variant="secondary" onClick={() => { setDraftSearch(""); setFilters({}); setPage(1); }}>清除</Button>
      </div>

      {error && <div className="notice notice-error" role="alert"><span>{error}</span><Button variant="secondary" size="sm" onClick={() => setReload((value) => value + 1)}>重试</Button></div>}
      {loading ? <div className="media-library-state">正在读取素材库…</div> : assets.length === 0 ? <div className="media-library-state">还没有生成素材。去调试台生成图片、视频或 PPT 后，结果会自动出现在这里。</div> : <div className="media-asset-grid">
        {assets.map((asset) => <AssetCard key={asset.id} asset={asset} canManage={canManage} deleting={deleting === asset.id} watermarking={watermarking === asset.id} onOpen={() => setSelected(asset)} onDelete={() => void remove(asset)} onRemoveWatermark={() => void removeWatermark(asset)} />)}
      </div>}

      <Pagination
        currentPage={page}
        pageSize={pageSize}
        total={total}
        pageSizes={PAGE_SIZE_OPTIONS}
        disabled={loading}
        onCurrentChange={setPage}
        onSizeChange={(nextPageSize) => { setPage(1); setPageSize(nextPageSize); }}
      />

      {selected && <AssetViewer asset={selected} onClose={() => setSelected(null)} />}
    </main>
  );
}

function AssetCard({ asset, canManage, deleting, watermarking, onOpen, onDelete, onRemoveWatermark }: { asset: MediaAsset; canManage: boolean; deleting: boolean; watermarking: boolean; onOpen: () => void; onDelete: () => void; onRemoveWatermark: () => void }) {
  return <article className="media-asset-card">
    <button type="button" className="media-asset-preview" onClick={onOpen} aria-label={`查看 ${asset.filename}`}>
      {asset.kind === "image" ? <img src={asset.content_url} alt={asset.filename} loading="lazy" /> : asset.kind === "video" ? <><video src={asset.content_url} preload="metadata" muted /><span className="media-play-badge"><Play size={18} fill="currentColor" aria-hidden="true" /></span></> : <div className="media-file-placeholder"><AssetIcon kind={asset.kind} /><strong>{kindLabels[asset.kind]}</strong><span>{asset.filename}</span></div>}
    </button>
    <div className="media-asset-info">
      <div className="media-asset-title" title={asset.filename}>{asset.filename}</div>
      <div className="media-asset-meta"><span>{kindLabels[asset.kind]} · {formatBytes(asset.size_bytes)}</span><span>{formatDate(asset.created_at)}</span></div>
      <div className="media-asset-source"><span>{asset.channel}</span><span>{asset.model || "—"}</span></div>
      <div className="media-asset-actions"><Button variant="secondary" size="sm" onClick={onOpen}><Download size={14} aria-hidden="true" />查看 / 下载</Button>{canManage && asset.kind === "image" && asset.metadata.watermark_free !== true && <Button variant="icon" size="sm" onClick={onRemoveWatermark} disabled={watermarking} title="去除生成水印并保存副本" aria-label={`去除 ${asset.filename} 的水印`}><Eraser size={15} aria-hidden="true" /></Button>}{canManage && <Button variant="icon" size="sm" onClick={onDelete} disabled={deleting} title="删除素材" aria-label={`删除 ${asset.filename}`}><Trash2 size={15} aria-hidden="true" /></Button>}</div>
    </div>
  </article>;
}

function AssetViewer({ asset, onClose }: { asset: MediaAsset; onClose: () => void }) {
  return <div className="media-viewer-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section className="media-viewer" role="dialog" aria-modal="true" aria-label={`查看 ${asset.filename}`}>
      <header className="media-viewer-header"><div><strong>{asset.filename}</strong><span>{kindLabels[asset.kind]} · {asset.channel} · {asset.model || "—"}</span></div><Button variant="secondary" onClick={onClose}>关闭</Button></header>
      <div className="media-viewer-body">{asset.kind === "image" ? <img src={asset.content_url} alt={asset.filename} /> : asset.kind === "video" ? <video src={asset.content_url} controls autoPlay /> : <div className="media-viewer-file"><AssetIcon kind={asset.kind} /><strong>{asset.filename}</strong><p>该文件类型暂不在浏览器内解析，下载后使用本机应用打开。</p><a className="ui-button ui-button--primary ui-button--md" href={asset.content_url} download={asset.filename}><Download size={15} aria-hidden="true" />下载文件</a></div>}</div>
    </section>
  </div>;
}
