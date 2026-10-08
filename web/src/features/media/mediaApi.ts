import { apiClient } from "../../api/client";
import type { PaginationMeta } from "../../app/data/pagination.constants";

export type MediaKind = "image" | "video" | "ppt" | "psd" | "archive" | "other";

export type MediaAsset = {
  id: string;
  actor: string;
  run_id?: string | null;
  conversation_id?: string | null;
  channel: string;
  model: string;
  kind: MediaKind;
  mime_type: string;
  filename: string;
  source_url?: string | null;
  size_bytes: number;
  metadata: Record<string, unknown>;
  created_at: number;
  updated_at: number;
  content_url: string;
  storage_status: "stored" | "remote";
};

export type MediaAssetPage = {
  data: MediaAsset[];
  pagination: PaginationMeta;
};

export type MediaAssetFilters = {
  kind?: MediaKind;
  channel?: string;
  search?: string;
};

export async function fetchMediaAssets(
  page = 1,
  pageSize = 24,
  filters: MediaAssetFilters = {},
  signal?: AbortSignal,
): Promise<MediaAssetPage> {
  const response = await apiClient.get<MediaAssetPage>("/media/assets", {
    params: { page, page_size: pageSize, ...filters },
    signal,
  });
  return response.data;
}

export async function deleteMediaAsset(id: string): Promise<void> {
  await apiClient.delete(`/media/assets/${encodeURIComponent(id)}`);
}

export async function saveWatermarkRemovedAsset(
  id: string,
  dataUrl: string,
  filename: string,
): Promise<MediaAsset> {
  const response = await apiClient.post<{ data: MediaAsset }>(
    `/media/assets/${encodeURIComponent(id)}/remove-watermark`,
    { data_url: dataUrl, filename },
  );
  return response.data.data;
}
