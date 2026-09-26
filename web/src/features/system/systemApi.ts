import { apiClient } from "../../api/client";

export type SystemInfo = {
  service: string;
  version: string;
  python_version: string;
  platform: string;
  python_implementation: string;
  schema_version: number;
  database: { present: boolean; bytes: number };
  runtime_state: { present: boolean; status: "ok" | "not_initialized" | "invalid" | "unreadable"; error?: string };
  channels: Array<{ slug: string; models_configured: boolean; accounts_configured: boolean }>;
};

export type StorageHealth = {
  status: "ok" | "degraded";
  database: { status: string; bytes: number; error?: string };
  runtime_state: { status: string; present: boolean; error?: string };
  disk: { status: string; total_bytes?: number; free_bytes?: number; used_bytes?: number; error?: string };
};

export type SystemMetrics = {
  from: string;
  to: string;
  requests: number;
  errors: number;
  error_rate: number;
  streams: number;
  avg_latency_ms: number;
  p95_latency_ms: number;
  channels: Array<{
    channel: string;
    requests: number;
    errors: number;
    error_rate: number;
    avg_latency_ms: number;
  }>;
  accounts: { total: number; enabled: number; available: number; runtime_observed: number };
};

type DataResponse<T> = { data: T };

export async function fetchSystemInfo(signal?: AbortSignal): Promise<SystemInfo> {
  const response = await apiClient.get<DataResponse<SystemInfo>>("/sysinfo", { signal });
  return response.data.data;
}

export async function fetchStorageHealth(signal?: AbortSignal): Promise<StorageHealth> {
  const response = await apiClient.get<DataResponse<StorageHealth>>("/storage/health", { signal });
  return response.data.data;
}

export async function fetchSystemMetrics(days: number, signal?: AbortSignal): Promise<SystemMetrics> {
  const response = await apiClient.get<DataResponse<SystemMetrics>>("/metrics", {
    params: { days },
    signal,
  });
  return response.data.data;
}

