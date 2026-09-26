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
  channels: ChannelOverview[];
  todos: OverviewTodo[];
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
