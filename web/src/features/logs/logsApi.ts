import { apiClient } from "../../api/client";
import { DEFAULT_PAGE_SIZE, type PaginationMeta } from "../../app/data/pagination.constants";

export type RequestLog = {
  id: number;
  ts: string;
  request_id: string;
  channel: string | null;
  key_id: number | null;
  model: string | null;
  upstream_model: string | null;
  route_alias: string | null;
  fallback_depth: number;
  status: number;
  error_kind: string | null;
  stream: number;
  prompt_tokens: number;
  completion_tokens: number;
  usage_reported: number;
  ttft_ms: number | null;
  latency_ms: number;
};

export type LogFilters = {
  request_id?: string;
  channel?: string;
  model?: string;
  status?: number;
  error_kind?: string;
  stream?: boolean;
  from?: string;
  to?: string;
};

export type LogPage = {
  data: RequestLog[];
  pagination: PaginationMeta;
};

export type ClearLogsResult = {
  request_logs_deleted: number;
  usage_daily_deleted: number;
  log_cutoff: string;
  usage_cutoff_day: string;
  log_retention_days: number;
  usage_retention_days: number;
};

export async function fetchLogs(
  page: number,
  filters: LogFilters,
  signal?: AbortSignal,
  pageSize = DEFAULT_PAGE_SIZE,
): Promise<LogPage> {
  const response = await apiClient.get<LogPage>("/logs", {
    params: { page, page_size: pageSize, ...filters },
    signal,
  });
  return response.data;
}

export async function clearExpiredLogs(): Promise<ClearLogsResult> {
  const response = await apiClient.post<{ data: ClearLogsResult }>("/logs/clear");
  return response.data.data;
}
