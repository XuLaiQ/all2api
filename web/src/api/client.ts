import axios, { type AxiosError, type AxiosInstance } from "axios";

const unauthorizedListeners = new Set<() => void>();

export function onUnauthorized(listener: () => void): () => void {
  unauthorizedListeners.add(listener);
  return () => unauthorizedListeners.delete(listener);
}

export class ApiClientError extends Error {
  constructor(
    message: string,
    readonly status: number | undefined,
    readonly body: unknown,
    readonly cause?: unknown,
  ) {
    super(message);
    this.name = "ApiClientError";
  }
}

function getErrorMessage(error: AxiosError<unknown>): string {
  if (typeof error.response?.data === "object" && error.response.data !== null) {
    const body = error.response.data as { detail?: unknown; message?: unknown };
    if (typeof body.detail === "string") return body.detail;
    if (typeof body.message === "string") return body.message;
    const apiError = (error.response.data as { error?: { message?: unknown } }).error;
    if (typeof apiError?.message === "string") return apiError.message;
  }

  return error.message || "请求失败，请稍后重试";
}

export function createApiClient(
  baseURL: string,
): AxiosInstance {
  const client = axios.create({
    baseURL,
    timeout: 30_000,
    withCredentials: true,
    headers: { Accept: "application/json" },
  });

  client.interceptors.response.use(
    (response) => response,
    (error: AxiosError<unknown>) => {
      if (error.response?.status === 401) {
        unauthorizedListeners.forEach((listener) => listener());
      }
      return Promise.reject(
        new ApiClientError(
          getErrorMessage(error),
          error.response?.status,
          error.response?.data,
          error,
        ),
      );
    },
  );

  return client;
}

export const apiClient = createApiClient(
  import.meta.env.VITE_ADMIN_API_BASE_URL || "/admin/api",
);
