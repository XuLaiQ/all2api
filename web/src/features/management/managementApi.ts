import { ApiClientError, apiClient } from "../../api/client";
import { DEFAULT_PAGE_SIZE, type PaginationMeta } from "../../app/data/pagination.constants";

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
  pagination: PaginationMeta;
};

export type PlaygroundRun = {
  id: string;
  conversation_id?: string | null;
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

export type PlaygroundConversationSummary = {
  id: string;
  title: string;
  channel: string;
  model: string;
  message_count: number;
  created_at: number;
  updated_at: number;
};

export type PlaygroundConversationMessage = {
  id: string;
  role: "user" | "assistant" | "error";
  content: string;
  model?: string;
  raw?: unknown;
  created_at: number;
};

export type PlaygroundConversation = PlaygroundConversationSummary & {
  messages: PlaygroundConversationMessage[];
};

export type PlaygroundResult = {
  run_id: string;
  conversation_id?: string;
  channel: string;
  model: string;
  status: string;
  response_status: number;
  response: unknown;
};

export type PlaygroundStreamEvent =
  | { type: "delta"; content: string }
  | { type: "error"; message: string; response_status?: number; error_code?: string }
  | (PlaygroundResult & { type: "done" });

export type PlaygroundSearchResult = {
  conversation_id?: string;
  status?: string;
  answer: string;
  sources: Array<{ title?: string; url: string; snippet?: string }>;
};

export type PlaygroundFileResult = {
  task_id: string;
  kind: "ppt" | "psd";
  status: "success" | "error";
  primary_url?: string;
  zip_url?: string;
  error?: string;
};

export async function runPlaygroundSearch(body: {
  channel?: string;
  model?: string;
  prompt: string;
}): Promise<PlaygroundSearchResult> {
  const response = await apiClient.post<DataResponse<PlaygroundSearchResult>>("/playground/search", body);
  return response.data.data;
}

export async function runPlaygroundFileTask(body: {
  channel?: string;
  model?: string;
  kind: "ppt" | "psd";
  prompt: string;
  base64_images?: string[];
}): Promise<PlaygroundFileResult> {
  const response = await apiClient.post<DataResponse<PlaygroundFileResult>>("/playground/editable-file", body);
  return response.data.data;
}

export async function fetchPlaygroundConversations(
  page = 1,
  pageSize = DEFAULT_PAGE_SIZE,
  signal?: AbortSignal,
): Promise<Page<PlaygroundConversationSummary>> {
  const response = await apiClient.get<Page<PlaygroundConversationSummary>>("/playground/conversations", {
    params: { page, page_size: pageSize },
    signal,
  });
  return response.data;
}

export async function fetchPlaygroundConversation(
  conversationId: string,
  signal?: AbortSignal,
): Promise<PlaygroundConversation> {
  const response = await apiClient.get<DataResponse<PlaygroundConversation>>(
    `/playground/conversations/${encodeURIComponent(conversationId)}`,
    { signal },
  );
  return response.data.data;
}

export async function deletePlaygroundConversation(
  conversationId: string,
): Promise<{ id: string; deleted: boolean; request_records_deleted: number }> {
  const response = await apiClient.delete<DataResponse<{
    id: string;
    deleted: boolean;
    request_records_deleted: number;
  }>>(`/playground/conversations/${encodeURIComponent(conversationId)}`);
  return response.data.data;
}

export async function fetchUsers(
  page = 1,
  pageSize = DEFAULT_PAGE_SIZE,
  signal?: AbortSignal,
): Promise<Page<ManagedUser>> {
  const response = await apiClient.get<Page<ManagedUser>>("/users", {
    params: { page, page_size: pageSize },
    signal,
  });
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

export async function fetchPlaygroundRuns(
  page = 1,
  pageSize = DEFAULT_PAGE_SIZE,
  signal?: AbortSignal,
): Promise<Page<PlaygroundRun>> {
  const response = await apiClient.get<Page<PlaygroundRun>>("/playground/runs", {
    params: { page, page_size: pageSize },
    signal,
  });
  return response.data;
}

export async function runPlayground(body: {
  channel: string;
  model: string;
  conversation_id?: string;
  messages: Array<{ role: "system" | "user" | "assistant"; content: string }>;
}): Promise<PlaygroundResult> {
  const response = await apiClient.post<DataResponse<PlaygroundResult>>("/playground/chat", body);
  return response.data.data;
}

function streamErrorMessage(body: unknown): string {
  if (!body || typeof body !== "object") return "调试请求失败";
  const payload = body as { detail?: unknown; message?: unknown; error?: unknown };
  if (typeof payload.detail === "string") return payload.detail;
  if (typeof payload.message === "string") return payload.message;
  if (payload.error && typeof payload.error === "object" && typeof (payload.error as { message?: unknown }).message === "string") {
    return (payload.error as { message: string }).message;
  }
  return "调试请求失败";
}

function parseStreamFrame(frame: string): unknown | null {
  const data = frame
    .replace(/\r\n/g, "\n")
    .split("\n")
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).replace(/^ /, ""))
    .join("\n")
    .trim();
  if (!data || data === "[DONE]") return null;
  try {
    return JSON.parse(data) as unknown;
  } catch {
    return null;
  }
}

export async function runPlaygroundStream(
  body: {
    channel: string;
    model: string;
    conversation_id?: string;
    messages: Array<{ role: "system" | "user" | "assistant"; content: string }>;
  },
  onEvent: (event: PlaygroundStreamEvent) => void,
  signal?: AbortSignal,
): Promise<PlaygroundResult> {
  const baseUrl = String(apiClient.defaults.baseURL || "/admin/api").replace(/\/$/, "");
  const response = await fetch(`${baseUrl}/playground/chat`, {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "text/event-stream",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ ...body, stream: true }),
    signal,
  });

  if (!response.ok) {
    let payload: unknown = null;
    try {
      payload = await response.json();
    } catch {
      payload = null;
    }
    throw new ApiClientError(streamErrorMessage(payload), response.status, payload);
  }
  if (!response.body) {
    throw new ApiClientError("浏览器不支持流式响应", response.status, null);
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let result: PlaygroundResult | null = null;

  const consumeFrame = (frame: string) => {
    const payload = parseStreamFrame(frame);
    if (!payload || typeof payload !== "object") return;
    const event = payload as PlaygroundStreamEvent;
    if (event.type === "delta" && typeof event.content === "string") {
      onEvent(event);
    } else if (event.type === "error" && typeof event.message === "string") {
      onEvent(event);
    } else if (event.type === "done") {
      result = event;
      onEvent(event);
    }
  };

  while (true) {
    const { done, value } = await reader.read();
    buffer += decoder.decode(value, { stream: !done });
    while (true) {
      const lf = buffer.indexOf("\n\n");
      const crlf = buffer.indexOf("\r\n\r\n");
      const boundary = lf >= 0 && (crlf < 0 || lf < crlf) ? lf : crlf;
      const boundaryLength = boundary === crlf ? 4 : 2;
      if (boundary < 0) break;
      consumeFrame(buffer.slice(0, boundary));
      buffer = buffer.slice(boundary + boundaryLength);
    }
    if (done) break;
  }
  if (buffer.trim()) consumeFrame(buffer);

  if (!result) {
    throw new ApiClientError("调试流未正常结束", response.status, null);
  }
  return result;
}
