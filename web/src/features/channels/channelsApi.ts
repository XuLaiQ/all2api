import { apiClient } from "../../api/client";
import type { ChannelOverview } from "../usage/usageApi";

export type ChannelRuntime = {
  model: string | null;
  state: string;
  consecutive_failures: number;
  breaker_streak: number;
  soft_streak: number;
  cooldown_until: number | null;
  breaker_until: number | null;
  last_status?: number | null;
  last_error_kind?: string | null;
  updated_at?: number;
  retry_after: number | null;
};

type DataResponse<T> = { data: T };

export async function fetchChannelRuntime(
  slug: string,
  signal?: AbortSignal,
): Promise<ChannelRuntime[]> {
  const response = await apiClient.get<DataResponse<{ channel: string; states: ChannelRuntime[] }>>(
    `/channels/${encodeURIComponent(slug)}/runtime`,
    { signal },
  );
  return response.data.data.states;
}

export type ChannelTestResult = {
  channel: string;
  status: "ok";
  latency_ms: number;
  model_count: number;
  tested_at: string;
};

export async function testChannel(
  slug: string,
  signal?: AbortSignal,
): Promise<ChannelTestResult> {
  const response = await apiClient.post<{ data: ChannelTestResult }>(
    `/channels/${encodeURIComponent(slug)}/test`,
    undefined,
    { signal },
  );
  return response.data.data;
}

export type { ChannelOverview };
