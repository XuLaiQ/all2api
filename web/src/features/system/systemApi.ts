import { apiClient } from "../../api/client";

export type SystemInfo = {
  service: string;
  version: string;
  go_version: string;
  platform: string;
  implementation: string;
  schema_version: number;
  database: { present: boolean; bytes: number };
  runtime_state: { present: boolean; status: "ok" | "not_initialized" | "invalid" | "unreadable"; error?: string };
  channels: Array<{
    slug: string;
    models_configured: boolean;
    accounts_configured: boolean;
    provision_configured: boolean;
  }>;
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

export type AdminSettings = {
  values: {
    log_retention_days: number;
    usage_retention_days: number;
  };
  sources: {
    log_retention_days: "environment" | "database";
    usage_retention_days: "environment" | "database";
  };
  mutable: string[];
  restart_required: boolean;
};

export type AdminSettingsPatch = Partial<AdminSettings["values"]>;

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

export async function fetchAdminSettings(signal?: AbortSignal): Promise<AdminSettings> {
  const response = await apiClient.get<DataResponse<AdminSettings>>("/settings", { signal });
  return response.data.data;
}

export async function updateAdminSettings(
  patch: AdminSettingsPatch,
  signal?: AbortSignal,
): Promise<AdminSettings> {
  const response = await apiClient.post<DataResponse<AdminSettings>>("/settings", patch, { signal });
  return response.data.data;
}
