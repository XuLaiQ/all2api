import { apiClient } from "../../api/client";
import type { ChannelOverview, UsageRow, UsageSummary } from "../usage/usageApi";

export type OverviewTodo = {
  id: string;
  code: string;
  severity: "warning" | "critical";
  channel: string;
  title: string;
  description: string;
  href: string;
};

export type OverviewPayload = {
  summary: UsageSummary;
  daily: { data: UsageRow[]; from: string; to: string };
  recent: RecentUsage;
  channels: ChannelOverview[];
  todos: OverviewTodo[];
};

export type RecentUsagePoint = {
  ts: string;
  prompt_tokens: number;
  completion_tokens: number;
  tokens: number;
  usage_reported_requests: number;
  usage_estimated_requests: number;
  usage_unknown_requests: number;
  requests: number;
};

export type RecentUsageSeries = {
  key_id: number | null;
  key_name: string;
  prompt_tokens: number;
  completion_tokens: number;
  tokens: number;
  usage_reported_requests: number;
  usage_estimated_requests: number;
  usage_unknown_requests: number;
  requests: number;
  points: RecentUsagePoint[];
};

export type RecentUsage = {
  metric: "tokens";
  usage_semantics: "reported_or_estimated_tokens";
  bucket: "hour";
  from: string;
  to: string;
  series: RecentUsageSeries[];
};

export async function fetchOverview(
  days: number,
  signal?: AbortSignal,
): Promise<OverviewPayload> {
  const response = await apiClient.get<{ data: OverviewPayload }>("/overview", {
    params: { days },
    signal,
  });
  return response.data.data;
}
