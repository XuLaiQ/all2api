import { apiClient } from "../../api/client";

export type AccountRecord = {
  id: string;
  channel: string;
  name: string;
  kind: string;
  tier: string | null;
  status: string;
  enabled: boolean;
  quota_used: number;
  quota_total: number;
  quota_unit: string;
  expires_at: number | null;
  cooldown_until: number | null;
  gateway_runtime: {
    state: "unobserved" | "closed" | "cooldown" | "breaker_open";
    success_count: number;
    fail_count: number;
    consecutive_failures: number;
    cooldown_until: number | null;
    breaker_until: number | null;
    last_status: number | null;
    last_error_kind: string | null;
    updated_at: number | null;
    retry_after: number | null;
  };
  updated_at: number;
};

export type AccountFilters = {
  channel?: string;
  status?: string;
  search?: string;
};

export type AccountPage = {
  data: AccountRecord[];
  pagination: { page: number; page_size: number; total: number; total_pages: number };
  last_synced: Record<string, number>;
  unconfigured_channels: string[];
};

export type AccountSyncResult = {
  data: {
    synced: number;
    unavailable_channels: string[];
    unconfigured_channels: string[];
  };
  unavailable_channels: string[];
  unconfigured_channels: string[];
  last_synced_at: number;
};

export async function fetchAccounts(
  page: number,
  filters: AccountFilters,
  signal?: AbortSignal,
): Promise<AccountPage> {
  const response = await apiClient.get<AccountPage>("/accounts", {
    params: { page, page_size: 50, ...filters },
    signal,
  });
  return response.data;
}

export async function syncAccounts(): Promise<AccountSyncResult> {
  const response = await apiClient.post<AccountSyncResult>("/accounts/sync");
  return response.data;
}

export type AccountOnboardingStart = {
  realm?: "cn" | "global";
  account_id?: string;
  name?: string;
  priority?: number;
  email_hint?: string;
};

export type AccountOnboardingSession = {
  channel: string;
  flow: "qr" | "oauth";
  status?: string;
  state?: string;
  auth_url?: string;
  session_id?: string;
  authorize_url?: string;
  account_id?: string;
  name?: string;
  qr_image_base64?: string;
  message?: string;
  error?: string;
  uid?: string | null;
  nickname?: string | null;
  added?: number;
  skipped?: number;
  refreshed?: number;
  errors?: string[];
};

export async function startAccountOnboarding(
  channel: string,
  payload: AccountOnboardingStart,
): Promise<AccountOnboardingSession> {
  const response = await apiClient.post<{ data: AccountOnboardingSession }>(
    `/accounts/${encodeURIComponent(channel)}/onboarding/start`,
    payload,
  );
  return response.data.data;
}

export async function pollAccountOnboarding(
  channel: string,
  params: { state?: string; realm?: string; region?: string; account_id?: string },
): Promise<AccountOnboardingSession> {
  const response = await apiClient.get<{ data: AccountOnboardingSession }>(
    `/accounts/${encodeURIComponent(channel)}/onboarding/poll`,
    { params },
  );
  return response.data.data;
}

export async function finishAccountOnboarding(
  channel: string,
  payload: { session_id?: string; callback?: string; tokens?: string[]; accounts?: Record<string, unknown>[] },
): Promise<AccountOnboardingSession> {
  const response = await apiClient.post<{ data: AccountOnboardingSession }>(
    `/accounts/${encodeURIComponent(channel)}/onboarding/finish`,
    payload,
  );
  return response.data.data;
}
