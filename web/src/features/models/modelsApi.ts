import { apiClient } from "../../api/client";
import { DEFAULT_PAGE_SIZE, type PaginationMeta } from "../../app/data/pagination.constants";

export type ModelRecord = {
  id: string;
  channel: string;
  upstream_id: string;
  public_id: string;
  display_name: string;
  kind: string;
  caps: string[];
  context_window: number | null;
  max_output: number | null;
  multiplier: number;
  enabled: boolean;
  display_name_locked: boolean;
  public_id_locked: boolean;
};

export type ModelFilters = {
  channel?: string;
  kind?: string;
  enabled?: boolean;
  search?: string;
};

export type ModelPage = {
  data: ModelRecord[];
  pagination: PaginationMeta;
  source: "observed_cache";
  facets: { kinds: string[] };
};

export type ModelRefreshResult = {
  source: "live";
  refreshed_at: number;
  total: number;
  channels: Array<{ channel: string; status: "ok" | "failed"; count?: number; error?: string }>;
};

const MODEL_CATALOG_TTL_MS = 5 * 60 * 1000;
const modelCatalogCache = new Map<string, { expiresAt: number; models: ModelRecord[] }>();
let modelCatalogCacheGeneration = 0;

export function invalidateModelCatalogCache(): void {
  modelCatalogCacheGeneration += 1;
  modelCatalogCache.clear();
}

export async function fetchModels(
  page: number,
  filters: ModelFilters,
  signal?: AbortSignal,
  pageSize = DEFAULT_PAGE_SIZE,
): Promise<ModelPage> {
  const response = await apiClient.get<ModelPage>("/models", {
    params: { page, page_size: pageSize, ...filters },
    signal,
  });
  return response.data;
}

export async function fetchAllModels(
  filters: ModelFilters,
  signal?: AbortSignal,
  pageSize = 200,
): Promise<ModelRecord[]> {
  if (signal?.aborted) throw new DOMException("The operation was aborted", "AbortError");
  const cacheKey = JSON.stringify([
    filters.channel ?? "",
    filters.kind ?? "",
    filters.enabled ?? null,
    filters.search ?? "",
    pageSize,
  ]);
  const generation = modelCatalogCacheGeneration;
  const cached = modelCatalogCache.get(cacheKey);
  if (cached && cached.expiresAt > Date.now()) return [...cached.models];

  try {
    const first = await fetchModels(1, filters, signal, pageSize);
    const models = [...first.data];
    for (let page = 2; page <= first.pagination.total_pages; page += 1) {
      const result = await fetchModels(page, filters, signal, pageSize);
      models.push(...result.data);
    }
    if (generation === modelCatalogCacheGeneration) {
      modelCatalogCache.set(cacheKey, { expiresAt: Date.now() + MODEL_CATALOG_TTL_MS, models });
    }
    return models;
  } catch (cause) {
    if (signal?.aborted) throw cause;
    if (cached && generation === modelCatalogCacheGeneration) return [...cached.models];
    throw cause;
  }
}

export function modelDisplayName(item: Pick<ModelRecord, "display_name" | "public_id" | "upstream_id">): string {
  return item.display_name.trim() || item.public_id || item.upstream_id;
}

export function modelOptionLabel(item: Pick<ModelRecord, "public_id" | "upstream_id">): string {
  return item.public_id || item.upstream_id;
}

export async function setModelEnabled(id: string, enabled: boolean): Promise<ModelRecord> {
  const response = await apiClient.patch<{ data: ModelRecord }>(
    `/models/${encodeURIComponent(id)}`,
    { enabled },
  );
  invalidateModelCatalogCache();
  return response.data.data;
}

export type ModelUpdate = {
  enabled?: boolean;
  display_name?: string;
  public_id?: string;
};

export async function updateModel(id: string, patch: ModelUpdate): Promise<ModelRecord> {
  const response = await apiClient.patch<{ data: ModelRecord }>(
    `/models/${encodeURIComponent(id)}`,
    patch,
  );
  invalidateModelCatalogCache();
  return response.data.data;
}

export async function refreshModels(channel?: string): Promise<ModelRefreshResult> {
  const response = await apiClient.post<{ data: ModelRefreshResult }>("/models/refresh", null, {
    params: channel ? { channel } : undefined,
  });
  invalidateModelCatalogCache();
  return response.data.data;
}
