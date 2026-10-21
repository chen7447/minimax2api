import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Coins, Hourglass, RefreshCw, Wallet } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip } from "@/components/ui/tooltip";
import {
  getCreditBreakdown,
  refreshAccountGrants,
  type CreditAccountItem,
  type CreditGrant,
} from "@/features/credits/credits-api";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { ErrorState, EmptyState } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber, formatRelative } from "@/shared/lib/format";

const DAY_MS = 86_400_000;

/**
 * Days until a grant expires, floored at 0. Grants without an expiry (the
 * upstream occasionally omits expire_at_ms) do not enter the distribution —
 * guessing a bucket for them would invent data.
 */
function grantDays(grant: CreditGrant, now: number): number | null {
  if (!grant.expiresAt) return null;
  const expires = Date.parse(grant.expiresAt);
  if (!Number.isFinite(expires)) return null;
  return Math.max(0, Math.ceil((expires - now) / DAY_MS));
}

/** Later buckets fade, echoing wb2api's opacity ramp: sooner = more visible. */
function bucketOpacity(days: number): number {
  return 0.3 + 0.7 * Math.max(0, Math.min(29, days - 1)) / 29;
}

/** A stable hue per account so its segments read as one account everywhere. */
function accountHue(seed: string): number {
  let hash = 0;
  for (let i = 0; i < seed.length; i += 1) {
    hash = (hash * 31 + seed.charCodeAt(i)) >>> 0;
  }
  return hash % 360;
}

type Bucket = { days: number; credits: number };

/** Groups every account's unspent grants into "expires in N days" buckets. */
function buildBuckets(items: CreditAccountItem[], now: number): Bucket[] {
  const map = new Map<number, number>();
  for (const item of items) {
    for (const grant of item.grants) {
      const days = grantDays(grant, now);
      if (days == null) continue;
      map.set(days, (map.get(days) ?? 0) + grant.remaining);
    }
  }
  return [...map.entries()]
    .map(([days, credits]) => ({ days, credits }))
    .sort((a, b) => a.days - b.days);
}

function formatCredits(value: number, locale: string): string {
  return formatNumber(value, locale, value % 1 === 0 ? 0 : 2);
}

export function CreditsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");

  const breakdownQuery = useQuery({
    queryKey: ["credits"],
    queryFn: getCreditBreakdown,
    staleTime: 15_000,
  });

  const grantsMutation = useMutation({
    mutationFn: (id: string) => refreshAccountGrants(id),
    onSuccess: () => {
      toast.success(t("credits.grantsRefreshed"));
      void queryClient.invalidateQueries({ queryKey: ["credits"] });
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
    },
  });

  const breakdown = breakdownQuery.data;
  const now = useMemo(() => Date.now(), [breakdown]);
  const buckets = useMemo(
    () => (breakdown ? buildBuckets(breakdown.items, now) : []),
    [breakdown, now],
  );
  const maxBucket = Math.max(1, ...buckets.map((bucket) => bucket.credits));

  const items = useMemo(() => {
    const list = breakdown?.items ?? [];
    if (!search.trim()) return list;
    const needle = search.trim().toLowerCase();
    return list.filter(
      (item) =>
        item.name.toLowerCase().includes(needle) ||
        item.id.toLowerCase().includes(needle),
    );
  }, [breakdown, search]);

  if (breakdownQuery.isError && !breakdown) {
    return (
      <div className="space-y-5">
        <PageHeader title={t("credits.title")} description={t("credits.description")} />
        <ErrorState
          message={(breakdownQuery.error as Error).message}
          onRetry={() => void breakdownQuery.refetch()}
        />
      </div>
    );
  }

  const loading = breakdownQuery.isPending;

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("credits.title")}
        description={t("credits.description")}
        actions={
          <Button
            variant="secondary"
            size="sm"
            disabled={breakdownQuery.isFetching}
            onClick={() => void breakdownQuery.refetch()}
          >
            <RefreshCw className={breakdownQuery.isFetching ? "animate-spin" : undefined} />
            {t("common.refresh")}
          </Button>
        }
      />

      <section className="grid gap-2 sm:grid-cols-3">
        <MetricCard
          icon={<Coins />}
          tone="text-quota-product-2"
          label={t("credits.totalRemaining")}
          value={loading ? null : formatCredits(breakdown?.totalRemaining ?? 0, i18n.language)}
          detail={t("credits.syncedAccounts", {
            synced: formatNumber(breakdown?.accountsSynced ?? 0),
            total: formatNumber(breakdown?.accountsTracked ?? 0),
          })}
        />
        <MetricCard
          icon={<Hourglass />}
          tone="text-quota-product-3"
          label={t("credits.expiringIn7Days")}
          value={loading ? null : formatCredits(breakdown?.expiringIn7Days ?? 0, i18n.language)}
          detail={t("credits.expiringDetail")}
        />
        <MetricCard
          icon={<Wallet />}
          tone="text-quota-product-1"
          label={t("credits.grantCount")}
          value={
            loading
              ? null
              : formatNumber(
                  (breakdown?.items ?? []).reduce((sum, item) => sum + item.grants.length, 0),
                )
          }
          detail={t("credits.grantCountDetail")}
        />
      </section>

      <DashboardPanel title={t("credits.expiryTitle")} contentClassName="min-h-0">
        {buckets.length === 0 ? (
          <p className="py-6 text-center text-xs text-muted-foreground">{t("credits.expiryEmpty")}</p>
        ) : (
          <div className="space-y-1">
            <div className="flex items-center gap-3 px-1 text-[10px] text-muted-foreground">
              <span className="w-20 shrink-0">{t("credits.daysColumn")}</span>
              <span className="flex-1 text-center">{t("credits.distributionColumn")}</span>
              <span className="w-20 shrink-0 text-right">{t("credits.creditsColumn")}</span>
            </div>
            <div className="max-h-72 space-y-1 overflow-y-auto pr-1">
              {buckets.map((bucket) => (
                <div key={bucket.days} className="flex items-center gap-3">
                  <span className="w-20 shrink-0 text-xs tabular-nums text-muted-foreground">
                    {bucket.days === 0 ? t("credits.expired") : t("credits.days", { count: bucket.days })}
                  </span>
                  <div
                    className="h-5 min-w-0 flex-1 overflow-hidden rounded-sm bg-secondary/40"
                    title={t("credits.bucketTooltip", {
                      days: bucket.days,
                      credits: formatCredits(bucket.credits, i18n.language),
                    })}
                  >
                    <div
                      className="h-full rounded-sm bg-quota-product-2 transition-[width] duration-300"
                      style={{
                        width: `${Math.max(2, (bucket.credits / maxBucket) * 100)}%`,
                        opacity: bucketOpacity(bucket.days),
                      }}
                    />
                  </div>
                  <span className="w-20 shrink-0 text-right text-xs font-medium tabular-nums">
                    {formatCredits(bucket.credits, i18n.language)}
                  </span>
                </div>
              ))}
            </div>
            <p className="px-1 pt-1 text-[10px] text-muted-foreground">{t("credits.expiryNote")}</p>
          </div>
        )}
      </DashboardPanel>

      <DataTableShell
        toolbar={
          <div className="relative min-w-52 flex-1 sm:max-w-64">
            <Input
              className="h-8 text-xs"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder={t("credits.search")}
            />
          </div>
        }
      >
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="whitespace-nowrap">{t("credits.account")}</TableHead>
              <TableHead className="whitespace-nowrap">{t("credits.balance")}</TableHead>
              <TableHead className="whitespace-nowrap">{t("credits.grantsRemaining")}</TableHead>
              <TableHead className="whitespace-nowrap">{t("credits.breakdown")}</TableHead>
              <TableHead className="whitespace-nowrap">{t("credits.syncedAt")}</TableHead>
              <TableHead className="w-10" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.map((item) => (
              <GrantRow
                key={item.id}
                item={item}
                busy={grantsMutation.isPending && grantsMutation.variables === item.id}
                onSync={() => grantsMutation.mutate(item.id)}
              />
            ))}
          </TableBody>
        </Table>
        {items.length === 0 && !loading ? <EmptyState label={t("common.empty")} /> : null}
      </DataTableShell>
    </div>
  );
}

function MetricCard({
  icon,
  tone,
  label,
  value,
  detail,
}: {
  icon: React.ReactNode;
  tone: string;
  label: string;
  /** Null renders the loading spinner; a number renders as-is. */
  value: string | null;
  detail: string;
}) {
  return (
    <article className="flex min-h-24 items-center gap-4 rounded-lg bg-card p-4">
      <span className={cn("flex size-9 shrink-0 items-center justify-center rounded-md bg-secondary/70 [&_svg]:size-4", tone)}>
        {icon}
      </span>
      <div className="min-w-0">
        <p className="truncate text-xs text-muted-foreground">{label}</p>
        <p className="mt-1 text-xl font-medium tabular-nums">
          {value === null ? <Spinner /> : value}
        </p>
        <p className="mt-0.5 truncate text-[10px] text-muted-foreground">{detail}</p>
      </div>
    </article>
  );
}

function GrantRow({
  item,
  busy,
  onSync,
}: {
  item: CreditAccountItem;
  busy: boolean;
  onSync: () => void;
}) {
  const { t, i18n } = useTranslation();
  const mainland = item.region === "cn";
  const hue = accountHue(item.id);
  const color = `hsl(${hue} 55% 55%)`;

  return (
    <TableRow>
      <TableCell>
        <div className="flex min-w-0 max-w-56 items-center gap-1.5">
          <span className="size-2 shrink-0 rounded-full" style={{ background: color }} />
          <span className="truncate text-xs font-medium" title={item.name}>
            {item.name}
          </span>
          {mainland ? (
            <span className="shrink-0 text-[10px] text-amber-700 dark:text-amber-300">
              {t("accounts.regionCN")}
            </span>
          ) : null}
        </div>
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs tabular-nums">
        {item.credit ? (
          <Tooltip label={t("accounts.creditBreakdown", { free: item.credit.free, purchased: item.credit.purchased })}>
            <span>{formatNumber(item.credit.total)}</span>
          </Tooltip>
        ) : (
          <span className="text-muted-foreground">{t("accounts.creditUnknown")}</span>
        )}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs tabular-nums">
        {formatCredits(item.grantsRemaining, i18n.language)}
      </TableCell>
      <TableCell className="min-w-0">
        {item.grants.length === 0 ? (
          <span className="text-xs text-muted-foreground">—</span>
        ) : (
          <div className="flex h-5 max-w-72 min-w-0 items-center gap-px overflow-hidden rounded-sm">
            {item.grants.map((grant, index) => {
              const days = grantDays(grant, Date.now());
              const share =
                item.grantsRemaining > 0 ? (grant.remaining / item.grantsRemaining) * 100 : 0;
              return (
                <Tooltip
                  key={index}
                  label={t("credits.grantTooltip", {
                    granted: formatCredits(grant.granted, i18n.language),
                    remaining: formatCredits(grant.remaining, i18n.language),
                    grantedAt: formatDateTime(grant.grantedAt, i18n.language),
                    expiresAt: days == null ? "—" : formatDateTime(grant.expiresAt, i18n.language),
                    days: days == null ? "—" : String(days),
                  })}
                >
                  <span
                    className="h-full min-w-0.5"
                    style={{
                      width: `${Math.max(1.5, share)}%`,
                      background: color,
                      opacity: days == null ? 1 : bucketOpacity(days),
                    }}
                  />
                </Tooltip>
              );
            })}
          </div>
        )}
      </TableCell>
      <TableCell className="whitespace-nowrap text-[10px] text-muted-foreground">
        {item.grantsSyncedAt ? formatRelative(item.grantsSyncedAt, i18n.language) : t("credits.neverSynced")}
      </TableCell>
      <TableCell>
        <Button
          variant="ghost"
          size="icon"
          className="size-7 text-muted-foreground"
          disabled={busy || mainland}
          aria-label={t("credits.syncOne")}
          onClick={onSync}
        >
          <RefreshCw className={cn("size-3.5", busy && "animate-spin")} />
        </Button>
      </TableCell>
    </TableRow>
  );
}
