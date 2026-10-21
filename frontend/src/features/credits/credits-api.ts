import { apiRequest } from "@/shared/api/client";
import type { AccountCredit } from "@/features/accounts/accounts-api";

/**
 * One batch of credits as the upstream's credit/details endpoint reports it.
 * Check-in payouts, promotional drops and purchased packs each arrive as a
 * separate grant with its own expiry, which is what the breakdown panel and
 * the expiry distribution are built from.
 */
export type CreditGrant = {
  grantedAt: string;
  expiresAt: string;
  granted: number;
  remaining: number;
};

export type CreditAccountItem = {
  id: string;
  name: string;
  region: string;
  enabled: boolean;
  status: string;
  credit: AccountCredit | null;
  /** Only grants with remaining > 0; expired ones are dropped server-side. */
  grants: CreditGrant[];
  grantsRemaining: number;
  grantsSyncedAt: string;
};

export type CreditBreakdown = {
  totalRemaining: number;
  expiringIn7Days: number;
  accountsSynced: number;
  accountsTracked: number;
  items: CreditAccountItem[];
};

/**
 * Reads the pool-wide credit breakdown from the server's cache. Never calls
 * upstream — freshness is per account, carried by grantsSyncedAt, and the
 * background poller keeps it moving.
 */
export function getCreditBreakdown(): Promise<CreditBreakdown> {
  return apiRequest<CreditBreakdown>("/admin/api/credits");
}

/**
 * Re-reads one account's grant breakdown from upstream. Cheap (one request),
 * which is what makes the per-row sync button usable while the pool-wide
 * refresh stays a background job.
 */
export function refreshAccountGrants(id: string): Promise<{ grants: CreditGrant[] }> {
  return apiRequest(`/admin/api/accounts/${id}/grants`, { method: "POST" });
}
