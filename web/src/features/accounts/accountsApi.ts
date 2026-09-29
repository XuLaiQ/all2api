import { apiClient } from "../../api/client";
import { DEFAULT_PAGE_SIZE, type PaginationMeta } from "../../app/data/pagination.constants";

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
  expires_at: number | string | null;
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
  updated_at: number | string;
};

export type AccountFilters = {
  channel?: string;
  status?: string;
  search?: string;
};

export type AccountPage = {
  data: AccountRecord[];
  pagination: PaginationMeta;
  unconfigured_channels: string[];
};

export async function fetchAccounts(
  page: number,
  filters: AccountFilters,
  signal?: AbortSignal,
  pageSize = DEFAULT_PAGE_SIZE,
): Promise<AccountPage> {
  const response = await apiClient.get<AccountPage>("/accounts", {
    params: { page, page_size: pageSize, ...filters },
    signal,
  });
  return response.data;
}

export async function setAccountEnabled(accountId: string, enabled: boolean): Promise<AccountRecord> {
  const response = await apiClient.patch<{ data: AccountRecord }>(
    `/accounts/${encodeURIComponent(accountId)}`,
    { enabled },
  );
  return response.data.data;
}

export async function deleteAccount(accountId: string): Promise<void> {
  await apiClient.delete(`/accounts/${encodeURIComponent(accountId)}`);
}

export async function refreshAccount(accountId: string): Promise<void> {
  await apiClient.post(`/accounts/${encodeURIComponent(accountId)}/refresh`);
}

export type AccountOnboardingStart = {
  realm?: "cn" | "global";
  account_id?: string;
  name?: string;
  priority?: number;
  email_hint?: string;
};

/**
 * A JSON-Schema subset returned by a channel manifest.  Account onboarding
 * deliberately keeps this type platform agnostic: adding a channel only
 * changes its manifest/provisioner, never this client DTO.
 */
export type ProvisionFieldSchema = {
  type?: "string" | "number" | "integer" | "boolean" | "array" | "object";
  title?: string;
  description?: string;
  format?: string;
  enum?: Array<string | number | boolean>;
  default?: unknown;
  minLength?: number;
  maxLength?: number;
  secret?: boolean;
  items?: ProvisionFieldSchema;
};

export type ProvisionObjectSchema = {
  type?: "object";
  title?: string;
  description?: string;
  properties?: Record<string, ProvisionFieldSchema>;
  required?: string[];
  additionalProperties?: boolean;
};

export type ProvisionFlowSpec = {
  id: string;
  kind: string;
  title?: string;
  description?: string;
  schema: ProvisionObjectSchema;
  supports?: {
    start?: boolean;
    poll?: boolean;
    complete?: boolean;
    import?: boolean;
    cancel?: boolean;
  };
  timeout_seconds?: number;
  requires_admin?: boolean;
};

export type ProvisionSchema = {
  channel: string;
  adapter_version?: string;
  flows: ProvisionFlowSpec[];
};

export type ProvisionEnvelope = {
  flow: string;
  payload: Record<string, unknown>;
  idempotency_key: string;
};

export type AccountOnboardingSession = {
  channel: string;
  flow?: string;
  status?: string;
  state?: string;
  auth_url?: string;
  session_id?: string;
  authorize_url?: string;
  account_id?: string;
  name?: string;
  qr_code?: string;
  qr_image_base64?: string;
  message?: string;
  error?: string;
  uid?: string | null;
  nickname?: string | null;
  added?: number;
  skipped?: number;
  refreshed?: number;
  errors?: string[];
  /** Target provision API may return a next action instead of a legacy flow. */
  next_step?: string;
  expires_at?: string | number | null;
  poll_after_seconds?: number;
  [key: string]: unknown;
};

type DataResponse<T> = { data: T };

export async function fetchProvisionSchema(
  channel: string,
  signal?: AbortSignal,
): Promise<ProvisionSchema> {
  const response = await apiClient.get<DataResponse<ProvisionSchema>>(
    `/channels/${encodeURIComponent(channel)}/provision-schema`,
    { signal },
  );
  return response.data.data;
}

export async function startProvision(
  channel: string,
  body: ProvisionEnvelope,
): Promise<AccountOnboardingSession> {
  const response = await apiClient.post<DataResponse<AccountOnboardingSession>>(
    `/channels/${encodeURIComponent(channel)}/accounts/provision/start`,
    body,
  );
  return response.data.data;
}

export async function pollProvision(
  channel: string,
  sessionId: string,
  signal?: AbortSignal,
): Promise<AccountOnboardingSession> {
  const response = await apiClient.get<DataResponse<AccountOnboardingSession>>(
    `/channels/${encodeURIComponent(channel)}/accounts/provision/${encodeURIComponent(sessionId)}`,
    { signal },
  );
  return response.data.data;
}

export async function completeProvision(
  channel: string,
  sessionId: string,
  body: { payload?: Record<string, unknown>; idempotency_key: string },
): Promise<AccountOnboardingSession> {
  const response = await apiClient.post<DataResponse<AccountOnboardingSession>>(
    `/channels/${encodeURIComponent(channel)}/accounts/provision/${encodeURIComponent(sessionId)}/complete`,
    body,
  );
  return response.data.data;
}

export async function importProvision(
  channel: string,
  body: ProvisionEnvelope,
): Promise<AccountOnboardingSession> {
  const response = await apiClient.post<DataResponse<AccountOnboardingSession>>(
    `/channels/${encodeURIComponent(channel)}/accounts/provision/import`,
    body,
  );
  return response.data.data;
}

export async function cancelProvision(
  channel: string,
  sessionId: string,
  idempotencyKey: string,
): Promise<AccountOnboardingSession> {
  const response = await apiClient.post<DataResponse<AccountOnboardingSession>>(
    `/channels/${encodeURIComponent(channel)}/accounts/provision/${encodeURIComponent(sessionId)}/cancel`,
    { idempotency_key: idempotencyKey },
  );
  return response.data.data;
}

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
