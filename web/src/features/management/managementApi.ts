import { apiClient } from "../../api/client";

type DataResponse<T> = { data: T };

export type ManagedUser = {
  username: string;
  role: "admin" | "viewer";
  enabled: boolean;
  created_at: number;
  updated_at: number;
};

export type Page<T> = {
  data: T[];
  pagination: { page: number; page_size: number; total: number; total_pages: number };
};

export type PlaygroundRun = {
  id: string;
  actor: string;
  channel: string;
  model: string;
  status: string;
  message_count: number;
  request_bytes: number;
  response_status: number | null;
  error_code: string | null;
  created_at: number;
  completed_at: number | null;
};

export type PlaygroundResult = {
  run_id: string;
  channel: string;
  model: string;
  status: string;
  response_status: number;
  response: unknown;
};

export async function fetchUsers(signal?: AbortSignal): Promise<Page<ManagedUser>> {
  const response = await apiClient.get<Page<ManagedUser>>("/users", { signal });
  return response.data;
}

export async function createUser(body: Pick<ManagedUser, "username" | "role" | "enabled">) {
  const response = await apiClient.post<DataResponse<ManagedUser>>("/users", body);
  return response.data.data;
}

export async function patchUser(username: string, body: Partial<Pick<ManagedUser, "role" | "enabled">>) {
  const response = await apiClient.patch<DataResponse<ManagedUser>>(`/users/${encodeURIComponent(username)}`, body);
  return response.data.data;
}

export async function deleteUser(username: string) {
  await apiClient.delete(`/users/${encodeURIComponent(username)}`);
}

export async function fetchPlaygroundRuns(signal?: AbortSignal): Promise<Page<PlaygroundRun>> {
  const response = await apiClient.get<Page<PlaygroundRun>>("/playground/runs", { signal });
  return response.data;
}

export async function runPlayground(body: {
  channel: string;
  model: string;
  messages: Array<{ role: "system" | "user" | "assistant"; content: string }>;
}): Promise<PlaygroundResult> {
  const response = await apiClient.post<DataResponse<PlaygroundResult>>("/playground/chat", body);
  return response.data.data;
}
