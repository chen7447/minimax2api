import { apiRequest } from "@/shared/api/client";

export type ModelDTO = {
  id: string;
  name: string;
  upstream: string;
  // The model identifier the upstream itself uses, when one exists. Chat
  // entries use it as the request's `model` field in direct mode; the video
  // entries use it as a generation parameter. It is not the same as `upstream`,
  // which is a local dispatch hint that never leaves this process.
  upstreamModel: string;
  type: "chat" | "image" | "video";
  enabled: boolean;
  builtin: boolean;
  description: string;
  requests: number;
  tokens: number;
  // Added by the most recent upstream refresh; the badge lasts one round.
  new: boolean;
  // The upstream no longer serves this id (a probe saw it substituted). The
  // entry stays visible for one round, then the next refresh removes it.
  unavailable: boolean;
};

export type ModelSyncResult = {
  upstream: string[];
  added: ModelDTO[];
  marked: ModelDTO[];
  removed: ModelDTO[];
  recovered: ModelDTO[];
  total: number;
};

export function listModels(): Promise<{ items: ModelDTO[] }> {
  return apiRequest("/admin/api/models");
}

export function refreshModels(): Promise<ModelSyncResult> {
  return apiRequest("/admin/api/models/refresh", { method: "POST" });
}

export function updateModel(id: string, payload: Partial<{ enabled: boolean; name: string; description: string }>): Promise<{ model: ModelDTO }> {
  return apiRequest(`/admin/api/models/${id}`, { method: "PATCH", body: payload });
}
