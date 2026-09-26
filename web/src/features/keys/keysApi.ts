import { apiClient } from "../../api/client";

export type ApiKeyRecord = {
  id: number;
  name: string;
  prefix: string;
  enabled: boolean;
  expires_at: number | null;
  channels: string[];
  models: string[];
  limit_rpm: number;
  created_at: number;
  last_used_at: number | null;
};

export type KeyInput = {
  name: string;
  channels: string[];
  models: string[];
  expires_at: number | null;
  limit_rpm: number;
};

export type KeyFilters = {
  search?: string;
  enabled?: boolean;
};

type ListResponse = {
  data: ApiKeyRecord[];
  pagination: { page: number; page_size: number; total: number; total_pages: number };
};

export async function fetchKeys(
  page: number,
  filters: KeyFilters = {},
  signal?: AbortSignal,
): Promise<ListResponse> {
  const response = await apiClient.get<ListResponse>("/keys", {
    params: { page, page_size: 50, ...filters },
    signal,
  });
  return response.data;
}

export async function createKey(input: KeyInput): Promise<{ data: ApiKeyRecord; key: string }> {
  const response = await apiClient.post<{ data: ApiKeyRecord; key: string }>("/keys", input);
  return response.data;
}

export async function updateKey(id: number, input: Partial<KeyInput> & { enabled?: boolean }): Promise<ApiKeyRecord> {
  const response = await apiClient.patch<{ data: ApiKeyRecord }>(`/keys/${id}`, input);
  return response.data.data;
}

export async function rotateKey(id: number): Promise<{ data: ApiKeyRecord; key: string }> {
  const response = await apiClient.post<{ data: ApiKeyRecord; key: string }>(`/keys/${id}/rotate`);
  return response.data;
}

export async function revokeKey(id: number): Promise<void> {
  await apiClient.delete(`/keys/${id}`);
}
