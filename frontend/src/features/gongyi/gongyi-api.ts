import { apiRequest } from "@/shared/api/client";

export type GongyiSnapshot = {
  target: number;
  total: number;
  enabled: number;
  cooling: number;
  exhausted: number;
  credits_total: number;
  daily_grant: number;
  reserve_days: number;
  runway_days: number;
  pct: number;
  capacity: number;
  remaining: number;
  gap: number;
};

export type GongyiToday = {
  requests: number;
  errors: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  avg_latency_ms: number;
};

export type GongyiPushRecord = {
  at: string;
  pct: number;
  remaining: number;
  http: number;
  ok: boolean;
  error?: string;
};

export type GongyiPush = {
  enabled: boolean;
  auto: boolean;
  worker_url: string;
  pool_id: string;
  capacity: number;
  daily_grant: number;
  reserve_days: number;
  target: number;
  minutes: number;
  next_at: string;
  last: GongyiPushRecord | null;
};

export type GongyiAudit = {
  id: string;
  created_at: string;
  account: string;
  model: string;
  status: number;
  latency_ms: number;
  prompt_tokens: number;
  completion_tokens: number;
};

export type GongyiOverview = {
  snapshot: GongyiSnapshot;
  today: GongyiToday;
  credit_used: number;
  credit_series: { date: string; credit_used: number }[];
  push: GongyiPush;
  online: Record<string, unknown>;
  requests: GongyiAudit[];
};

export type GongyiAccountToday = {
  requests: number;
  errors: number;
  tokens: number;
  latency_ms: number;
};

export type GongyiAccount = {
  id: string;
  name: string;
  region: string;
  credits: number;
  enabled: boolean;
  status: string;
  cooldown: string;
  exhausted_at: string;
  last_used_at: string;
  last_error: string;
  success: number;
  fail: number;
  today: GongyiAccountToday | null;
  credit_synced_at?: string;
};

export type GongyiConfig = {
  enabled: boolean;
  worker_url: string;
  admin_key: string;
  pool_id: string;
  capacity: number;
  daily_grant: number;
  reserve_days: number;
  target: number;
  push_minutes: number;
  auto_push: boolean;
  key_id: string;
  refresh_per_tick: number;
  refresh_minutes: number;
};

export type GongyiSelectResult = {
  ok: boolean;
  dry_run: boolean;
  candidates: number;
  refreshed: boolean;
  refresh_failed: number;
  qualifying: number;
  selected: number;
  cutoff_credits: number;
  target: number;
  mode: string;
  excluded_off: number;
  gap: number;
  pool_total?: number;
  accounts: { id: string; name: string; region: string; credits: number; status: string }[];
};

export function getGongyiOverview(): Promise<GongyiOverview> {
  return apiRequest("/admin/api/gongyi/overview");
}

export function getGongyiAccounts(): Promise<{ accounts: GongyiAccount[]; total: number }> {
  return apiRequest("/admin/api/gongyi/accounts");
}

export function getGongyiConfig(): Promise<GongyiConfig> {
  return apiRequest("/admin/api/gongyi/config");
}

export function saveGongyiConfig(payload: Partial<GongyiConfig>): Promise<{ ok: boolean; admin_key: string; worker_url: string }> {
  return apiRequest("/admin/api/gongyi/config", { method: "POST", body: payload });
}

export function selectGongyi(payload: {
  limit: number;
  min_credits: number;
  mode: "replace" | "add";
  dry_run: boolean;
  refresh: boolean;
  key_id?: string;
}): Promise<GongyiSelectResult> {
  return apiRequest("/admin/api/gongyi/select", { method: "POST", body: payload });
}

export function toggleGongyiAccount(id: string, inPool: boolean): Promise<{ ok: boolean }> {
  return apiRequest(`/admin/api/gongyi/accounts/${encodeURIComponent(id)}`, {
    method: "POST",
    body: { in_pool: inPool },
  });
}

export function refreshGongyi(payload: { ids?: string[]; n?: number }): Promise<{ ok: boolean; refreshed: number; failed: number }> {
  return apiRequest("/admin/api/gongyi/refresh", { method: "POST", body: payload });
}

export function pushGongyi(): Promise<{ ok: boolean; result: GongyiPushRecord }> {
  return apiRequest("/admin/api/gongyi/push", { method: "POST" });
}

export function selfCheckGongyi(): Promise<{
  ok: boolean;
  result: GongyiPushRecord;
  online: Record<string, unknown>;
  diff: number | null;
}> {
  return apiRequest("/admin/api/gongyi/self_check", { method: "POST" });
}

export function getGongyiUsage(days = 7): Promise<{
  days: number;
  series: { date: string; requests: number; errors: number; tokens: number }[];
  credit_series: { date: string; credit_used: number }[];
}> {
  return apiRequest(`/admin/api/gongyi/usage?days=${days}`);
}
