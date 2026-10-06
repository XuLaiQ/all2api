import { apiClient } from "../../api/client";

export type UsageSummary = {
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  tokens: number;
  usage_reported_requests: number;
  usage_estimated_requests: number;
  usage_unknown_requests: number;
  credits: number | null;
  credits_available: boolean;
  from: string;
  to: string;
};

export type UsageRow = {
  day?: string;
  channel?: string;
  model?: string;
  key_id?: number;
  key_name?: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  tokens: number;
  usage_reported_requests: number;
  usage_estimated_requests: number;
  usage_unknown_requests: number;
  credits: number | null;
  credits_available: boolean;
};

export type UsageGroup = "daily" | "channel" | "model" | "key";

type DataResponse<T> = { data: T };

const usagePaths: Record<UsageGroup, string> = {
  daily: "/stats/daily",
  channel: "/stats/by-channel",
  model: "/stats/by-model",
  key: "/stats/by-key",
};

export async function fetchUsageSummary(
  days: number,
  signal?: AbortSignal,
): Promise<UsageSummary> {
  const response = await apiClient.get<DataResponse<UsageSummary>>("/stats/summary", {
    params: { days },
    signal,
  });
  return response.data.data;
}

export async function fetchUsageRows(
  group: UsageGroup,
  days: number,
  signal?: AbortSignal,
): Promise<UsageRow[]> {
  const response = await apiClient.get<DataResponse<UsageRow[]>>(usagePaths[group], {
    params: { days },
    signal,
  });
  return response.data.data;
}

export type ChannelOverview = {
  slug: string;
  name: string;
  adapter: string;
  enabled: boolean;
  management_enabled?: boolean;
  data_plane_configured?: boolean;
  state: string;
  accounts_configured: boolean;
  provision_configured?: boolean;
  account_config?: {
    configured: boolean;
    required_env: string[];
    missing_env: string[];
  };
  protocols: string[];
  caps: string[];
  runtime: {
    state: string;
    consecutive_failures: number;
    cooldown_until: number | null;
    breaker_until: number | null;
    last_status?: number | null;
    last_error_kind?: string | null;
    updated_at?: number;
    retry_after: number | null;
  };
};

export async function fetchChannels(signal?: AbortSignal): Promise<ChannelOverview[]> {
  const response = await apiClient.get<DataResponse<ChannelOverview[]>>("/channels", {
    signal,
  });
  return response.data.data;
}
