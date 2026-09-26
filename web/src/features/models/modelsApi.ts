import { apiClient } from "../../api/client";

export type ModelRecord = {
  id: string;
  channel: string;
  upstream_id: string;
  display_name: string;
  kind: string;
  caps: string[];
  context_window: number | null;
  max_output: number | null;
  multiplier: number;
  enabled: boolean;
};

export type ModelFilters = {
  channel?: string;
  kind?: string;
  enabled?: boolean;
  search?: string;
};

export type ModelPage = {
  data: ModelRecord[];
  pagination: { page: number; page_size: number; total: number; total_pages: number };
  source: "observed_cache";
  facets: { kinds: string[] };
};

export async function fetchModels(
  page: number,
  filters: ModelFilters,
  signal?: AbortSignal,
): Promise<ModelPage> {
  const response = await apiClient.get<ModelPage>("/models", {
    params: { page, page_size: 50, ...filters },
    signal,
  });
  return response.data;
}

export async function setModelEnabled(id: string, enabled: boolean): Promise<ModelRecord> {
  const response = await apiClient.patch<{ data: ModelRecord }>(
    `/models/${encodeURIComponent(id)}`,
    { enabled },
  );
  return response.data.data;
}
