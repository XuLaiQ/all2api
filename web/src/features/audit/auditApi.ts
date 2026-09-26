import { apiClient } from "../../api/client";

export type AuditRecord = {
  id: number;
  ts: string;
  actor: string;
  action: string;
  target: string;
  detail: string;
};

export type AuditFilters = {
  actor?: string;
  action?: string;
  target?: string;
  from?: string;
  to?: string;
};

export type AuditPage = {
  data: AuditRecord[];
  pagination: { page: number; page_size: number; total: number; total_pages: number };
};

export async function fetchAuditLogs(
  page: number,
  filters: AuditFilters,
  signal?: AbortSignal,
): Promise<AuditPage> {
  const response = await apiClient.get<AuditPage>("/audit-logs", {
    params: { page, page_size: 50, ...filters },
    signal,
  });
  return response.data;
}
