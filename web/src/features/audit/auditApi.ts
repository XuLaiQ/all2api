import { apiClient } from "../../api/client";
import { DEFAULT_PAGE_SIZE, type PaginationMeta } from "../../app/data/pagination.constants";

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
  pagination: PaginationMeta;
};

export async function fetchAuditLogs(
  page: number,
  filters: AuditFilters,
  signal?: AbortSignal,
  pageSize = DEFAULT_PAGE_SIZE,
): Promise<AuditPage> {
  const response = await apiClient.get<AuditPage>("/audit-logs", {
    params: { page, page_size: pageSize, ...filters },
    signal,
  });
  return response.data;
}
