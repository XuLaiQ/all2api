import { apiClient } from "../../api/client";

export type RouteTarget = { position: number; channel: string; model: string; weight: number };

export type ModelRoute = {
  alias: string;
  strategy: "priority";
  enabled: boolean;
  created_at: number;
  targets: RouteTarget[];
};

export type RouteInput = {
  strategy: "priority";
  enabled: boolean;
  targets: { channel: string; model: string }[];
};

export type RegisteredChannel = { slug: string; name: string; enabled: boolean };

export async function fetchRoutes(signal?: AbortSignal): Promise<ModelRoute[]> {
  const response = await apiClient.get<{ data: ModelRoute[]; total: number }>("/routes", { signal });
  return response.data.data;
}

export async function fetchRouteChannels(signal?: AbortSignal): Promise<RegisteredChannel[]> {
  const response = await apiClient.get<{ data: RegisteredChannel[] }>("/channels/adapters", { signal });
  return response.data.data;
}

export async function saveRoute(alias: string, input: RouteInput): Promise<ModelRoute> {
  const response = await apiClient.put<{ data: ModelRoute }>(`/routes/${encodeURIComponent(alias)}`, input);
  return response.data.data;
}

export async function deleteRoute(alias: string): Promise<void> {
  await apiClient.delete(`/routes/${encodeURIComponent(alias)}`);
}
