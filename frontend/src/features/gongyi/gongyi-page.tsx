import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw, Send, Settings2, ShieldCheck, UserPlus } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input, Label } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  getGongyiAccounts,
  getGongyiConfig,
  getGongyiOverview,
  getGongyiUsage,
  pushGongyi,
  refreshGongyi,
  saveGongyiConfig,
  selectGongyi,
  selfCheckGongyi,
  toggleGongyiAccount,
  type GongyiConfig,
} from "@/features/gongyi/gongyi-api";
import { listClientKeys } from "@/features/client-keys/client-keys-api";
import { errorMessage } from "@/shared/api/client";
import { EmptyState } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { formatDateTime, formatNumber, formatRelative, formatTokens } from "@/shared/lib/format";

function pct(value: number): string {
  return `${(value * 100).toFixed(1)}%`;
}

function statusTone(status: string): string {
  switch (status) {
    case "active":
      return "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300";
    case "cooldown":
      return "bg-amber-500/10 text-amber-700 dark:text-amber-300";
    case "exhausted":
      return "bg-red-500/10 text-red-700 dark:text-red-300";
    case "invalid":
      return "bg-red-500/10 text-red-700 dark:text-red-300";
    default:
      return "bg-muted text-muted-foreground";
  }
}

// 水池标签：成对同长、慢池在前；每次打开/手动刷新随机取一组，30s 自动刷新不换。
const POOL_LABELS = [
  ["月盈", "雨润"],
  ["甘霖", "细雨"],
  ["月满", "时雨"],
  ["月映深潭", "雨润万物"],
  ["深潭蓄月", "细雨润物"],
  ["月到中天", "雨积成潭"],
];

function pickPoolPair(): [string, string] {
  const pair = POOL_LABELS[Math.floor(Math.random() * POOL_LABELS.length)];
  return [pair[0], pair[1]];
}

export function GongyiPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [cfgOpen, setCfgOpen] = useState(false);
  const [draft, setDraft] = useState<GongyiConfig | null>(null);
  const [limit, setLimit] = useState(42);
  const [minCredits, setMinCredits] = useState(1);
  const [poolPair, setPoolPair] = useState<[string, string]>(pickPoolPair);

  const overviewQuery = useQuery({ queryKey: ["gongyi", "overview"], queryFn: getGongyiOverview, refetchInterval: 30_000 });
  const accountsQuery = useQuery({ queryKey: ["gongyi", "accounts"], queryFn: getGongyiAccounts, refetchInterval: 30_000 });
  const configQuery = useQuery({ queryKey: ["gongyi", "config"], queryFn: getGongyiConfig });
  const usageQuery = useQuery({ queryKey: ["gongyi", "usage"], queryFn: () => getGongyiUsage(7), refetchInterval: 60_000 });
  const keysQuery = useQuery({ queryKey: ["client-keys"], queryFn: listClientKeys });

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["gongyi"] });
    void queryClient.invalidateQueries({ queryKey: ["dashboard"] });
    void queryClient.invalidateQueries({ queryKey: ["credits"] });
  };

  function manualRefresh() {
    setPoolPair(pickPoolPair());
    void overviewQuery.refetch();
    void accountsQuery.refetch();
    void usageQuery.refetch();
  }

  const selectMutation = useMutation({
    mutationFn: (dryRun: boolean) =>
      selectGongyi({ limit, min_credits: minCredits, mode: "replace", dry_run: dryRun, refresh: true }),
    onSuccess: (result) => {
      toast.success(
        result.dry_run
          ? `预览：达标 ${result.qualifying}，入选 ${result.selected}，缺口 ${result.gap}`
          : `已纳入 ${result.selected} 个，缺口 ${result.gap}`,
      );
      invalidate();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const refreshMutation = useMutation({
    mutationFn: () => refreshGongyi({ n: 2 }),
    onSuccess: (result) => {
      toast.success(`轮转刷新：成功 ${result.refreshed}，失败 ${result.failed}`);
      invalidate();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const pushMutation = useMutation({
    mutationFn: pushGongyi,
    onSuccess: (result) => {
      if (result.ok) toast.success(`推送成功：${pct(result.result.pct)} → ${result.result.remaining}`);
      else toast.error(`推送失败：${result.result.error ?? "未知错误"}`);
      invalidate();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const selfCheckMutation = useMutation({
    mutationFn: selfCheckGongyi,
    onSuccess: (result) => {
      toast.success(
        `自检：本地 ${pct(result.result.pct)} / 线上 ${String(result.online.daily ?? "-")}% / 差异 ${result.diff ?? "-"}`,
      );
      invalidate();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const toggleMutation = useMutation({
    mutationFn: ({ id, inPool }: { id: string; inPool: boolean }) => toggleGongyiAccount(id, inPool),
    onSuccess: () => {
      toast.success("公益池已更新");
      invalidate();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const singleRefreshMutation = useMutation({
    mutationFn: (id: string) => refreshGongyi({ ids: [id] }),
    onSuccess: () => {
      toast.success("额度已刷新");
      invalidate();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const saveConfigMutation = useMutation({
    mutationFn: (payload: Partial<GongyiConfig>) => saveGongyiConfig(payload),
    onSuccess: () => {
      toast.success("配置已保存");
      setCfgOpen(false);
      invalidate();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const overview = overviewQuery.data;
  const snapshot = overview?.snapshot;
  const accounts = accountsQuery.data?.accounts ?? [];
  const usage = usageQuery.data;
  const onlineDaily = overview?.online && typeof overview.online.daily === "number" ? overview.online.daily : null;

  function openConfig() {
    const cfg = configQuery.data;
    if (cfg) {
      setDraft({ ...cfg });
      setCfgOpen(true);
    } else {
      void configQuery.refetch();
    }
  }

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("nav.gongyi")}
        description="仅 minimax-m3、显式关闭 thinking；只向 translate.7447.site 推送聚合百分比。"
      />

      <div className="flex flex-wrap items-center gap-2">
        <Button variant="outline" size="sm" onClick={manualRefresh} disabled={overviewQuery.isFetching}>
          <RefreshCw className="size-3.5" /> 刷新
        </Button>
        <Button variant="outline" size="sm" onClick={() => selectMutation.mutate(true)} disabled={selectMutation.isPending}>
          <UserPlus className="size-3.5" /> 预览纳入
        </Button>
        <Button size="sm" onClick={() => selectMutation.mutate(false)} disabled={selectMutation.isPending}>
          <UserPlus className="size-3.5" /> 一键纳入满额号
        </Button>
        <Button variant="outline" size="sm" onClick={() => refreshMutation.mutate()} disabled={refreshMutation.isPending}>
          <RefreshCw className="size-3.5" /> 轮转刷新额度
        </Button>
        <Button variant="outline" size="sm" onClick={() => pushMutation.mutate()} disabled={pushMutation.isPending}>
          <Send className="size-3.5" /> 立即推送
        </Button>
        <Button variant="outline" size="sm" onClick={() => selfCheckMutation.mutate()} disabled={selfCheckMutation.isPending}>
          <ShieldCheck className="size-3.5" /> 自检
        </Button>
        <Button variant="outline" size="sm" onClick={openConfig}>
          <Settings2 className="size-3.5" /> 配置
        </Button>
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <div className="rounded-xl border bg-card p-4 lg:col-span-2">
          <h3 className="text-sm font-medium">公益池总览</h3>
          {overviewQuery.isPending || !snapshot ? (
            <div className="flex h-32 items-center justify-center"><Spinner className="size-5" /></div>
          ) : (
            <>
              <div className="mt-3 grid grid-cols-2 gap-3 text-sm md:grid-cols-4">
                <div><p className="text-xs text-muted-foreground">目标 / 实有</p><p className="text-lg font-semibold tabular-nums">{snapshot.target} / {snapshot.total}</p></div>
                <div><p className="text-xs text-muted-foreground">可用 / 冷却 / 耗尽</p><p className="text-lg font-semibold tabular-nums">{snapshot.enabled} / {snapshot.cooling} / {snapshot.exhausted}</p></div>
                <div><p className="text-xs text-muted-foreground">总余额</p><p className="text-lg font-semibold tabular-nums">{formatNumber(snapshot.credits_total, i18n.language)}</p></div>
                <div><p className="text-xs text-muted-foreground">缺口</p><p className="text-lg font-semibold tabular-nums">{snapshot.gap}</p></div>
              </div>
              <div className="mt-4">
                <div className="flex items-center justify-between text-xs text-muted-foreground">
                  <span>{poolPair[1]} {pct(snapshot.pct)}</span>
                  <span>remaining {snapshot.remaining} / {snapshot.capacity}</span>
                </div>
                <div className="mt-1 h-2 overflow-hidden rounded-full bg-muted">
                  <div className="h-full rounded-full bg-emerald-500 transition-all" style={{ width: `${Math.min(100, snapshot.pct * 100)}%` }} />
                </div>
              </div>
            </>
          )}
        </div>

        <div className="rounded-xl border bg-card p-4">
          <h3 className="text-sm font-medium">联动 translate.7447.site</h3>
          {overview ? (
            <div className="mt-3 space-y-2 text-sm">
              <p className="flex items-center justify-between"><span className="text-muted-foreground">状态</span><span>{!overview.push.enabled ? "未启用" : overview.push.last ? (overview.push.last.ok ? "正常" : "失败") : "等待首轮"}</span></p>
              <p className="flex items-center justify-between"><span className="text-muted-foreground">本地 / 线上{poolPair[1]}</span><span className="tabular-nums">{pct(overview.push.last?.pct ?? 0)} / {onlineDaily == null ? "-" : `${onlineDaily}%`}</span></p>
              <p className="flex items-center justify-between"><span className="text-muted-foreground">上次 / 下次</span><span className="text-xs">{overview.push.last?.at ?? "-"} / {overview.push.next_at}</span></p>
              {overview.push.last && !overview.push.last.ok ? (
                <p className="text-xs text-red-500">{overview.push.last.error}</p>
              ) : (
                <p className="text-xs text-muted-foreground">只推送聚合百分比，Worker 侧不出现账号/渠道信息。</p>
              )}
            </div>
          ) : (
            <div className="flex h-32 items-center justify-center"><Spinner className="size-5" /></div>
          )}
        </div>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <div className="rounded-xl border bg-card p-4">
          <h3 className="text-sm font-medium">今日用量</h3>
          {overview ? (
            <div className="mt-3 grid grid-cols-2 gap-3 text-sm">
              <div><p className="text-xs text-muted-foreground">请求 / 失败</p><p className="text-lg font-semibold tabular-nums">{formatNumber(overview.today.requests, i18n.language)} / {formatNumber(overview.today.errors, i18n.language)}</p></div>
              <div><p className="text-xs text-muted-foreground">tokens</p><p className="text-lg font-semibold tabular-nums">{formatTokens(overview.today.total_tokens, i18n.language)}</p></div>
              <div><p className="text-xs text-muted-foreground">额度消耗</p><p className="text-lg font-semibold tabular-nums">{formatNumber(overview.credit_used, i18n.language)}</p></div>
              <div><p className="text-xs text-muted-foreground">平均延迟</p><p className="text-lg font-semibold tabular-nums">{formatNumber(overview.today.avg_latency_ms, i18n.language)} ms</p></div>
            </div>
          ) : (
            <div className="flex h-24 items-center justify-center"><Spinner className="size-5" /></div>
          )}
        </div>

        <div className="rounded-xl border bg-card p-4">
          <h3 className="text-sm font-medium">近 7 日</h3>
          {usage ? (
            <div className="mt-3 space-y-1 text-xs">
              {usage.series.map((row) => {
                const credit = usage.credit_series.find((item) => item.date === row.date)?.credit_used ?? 0;
                return (
                  <div key={row.date} className="flex items-center justify-between tabular-nums">
                    <span className="text-muted-foreground">{row.date.slice(5)}</span>
                    <span>{row.requests} 次 · {formatTokens(row.tokens, i18n.language)} tok · {formatNumber(credit, i18n.language)} 额度</span>
                  </div>
                );
              })}
            </div>
          ) : (
            <div className="flex h-24 items-center justify-center"><Spinner className="size-5" /></div>
          )}
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3 rounded-xl border bg-card p-4 text-sm">
        <span className="text-muted-foreground">纳入参数</span>
        <Label className="flex items-center gap-2">目标 <Input className="h-8 w-20" type="number" value={limit} onChange={(event) => setLimit(Number(event.target.value) || 42)} /></Label>
        <Label className="flex items-center gap-2">最低额度 <Input className="h-8 w-24" type="number" value={minCredits} onChange={(event) => setMinCredits(Number(event.target.value) || 1)} /></Label>
        <span className="text-xs text-muted-foreground">只选已启用、额度 ≥ 门槛的账号，按额度降序；replace 会清除未入选账号的公益标记。</span>
      </div>

      <DataTableShell toolbar={<span className="text-xs text-muted-foreground">公益账号 {accounts.length}</span>}>
        {accountsQuery.isPending ? (
          <div className="flex h-48 items-center justify-center"><Spinner className="size-5" /></div>
        ) : accounts.length === 0 ? (
          <EmptyState label="公益池为空——点上方「一键纳入满额号」" />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>账号</TableHead>
                <TableHead>区</TableHead>
                <TableHead>状态</TableHead>
                <TableHead className="text-right">额度</TableHead>
                <TableHead className="text-right">今日请求</TableHead>
                <TableHead className="text-right">今日 tokens</TableHead>
                <TableHead>最后使用</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {accounts.map((account) => (
                <TableRow key={account.id}>
                  <TableCell>
                    <p className="text-xs font-medium">{account.name}</p>
                    <p className="font-mono text-[10px] text-muted-foreground">{account.id}</p>
                  </TableCell>
                  <TableCell className="text-xs">{account.region || "-"}</TableCell>
                  <TableCell>
                    <span className={`inline-flex h-5 items-center rounded-full px-2 text-[11px] ${statusTone(account.status)}`}>{account.status}</span>
                    {account.last_error ? <p className="mt-1 max-w-48 truncate text-[10px] text-red-500" title={account.last_error}>{account.last_error}</p> : null}
                  </TableCell>
                  <TableCell className="text-right text-xs tabular-nums">{formatNumber(account.credits, i18n.language)}</TableCell>
                  <TableCell className="text-right text-xs tabular-nums">{account.today?.requests ?? 0}</TableCell>
                  <TableCell className="text-right text-xs tabular-nums">{formatTokens(account.today?.tokens ?? 0, i18n.language)}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">{account.last_used_at ? formatRelative(account.last_used_at) : "-"}</TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button variant="ghost" size="sm" onClick={() => singleRefreshMutation.mutate(account.id)} disabled={singleRefreshMutation.isPending}>刷新额度</Button>
                      <Button variant="ghost" size="sm" onClick={() => toggleMutation.mutate({ id: account.id, inPool: false })} disabled={toggleMutation.isPending}>移出</Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </DataTableShell>

      <DataTableShell toolbar={<span className="text-xs text-muted-foreground">最近公益请求</span>}>
        {overview && overview.requests.length > 0 ? (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>时间</TableHead>
                <TableHead>账号</TableHead>
                <TableHead>模型</TableHead>
                <TableHead>状态</TableHead>
                <TableHead className="text-right">延迟</TableHead>
                <TableHead className="text-right">tokens</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {overview.requests.slice(0, 50).map((audit) => (
                <TableRow key={audit.id}>
                  <TableCell className="text-xs">{formatDateTime(audit.created_at)}</TableCell>
                  <TableCell className="text-xs">{audit.account}</TableCell>
                  <TableCell className="font-mono text-xs">{audit.model}</TableCell>
                  <TableCell className={`text-xs ${audit.status >= 400 ? "text-red-500" : "text-emerald-600"}`}>{audit.status}</TableCell>
                  <TableCell className="text-right text-xs tabular-nums">{audit.latency_ms} ms</TableCell>
                  <TableCell className="text-right text-xs tabular-nums">{formatTokens(audit.prompt_tokens + audit.completion_tokens, i18n.language)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : (
          <EmptyState label="暂无公益请求" />
        )}
      </DataTableShell>

      <Dialog open={cfgOpen} onOpenChange={setCfgOpen}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>公益联动配置</DialogTitle>
            <DialogDescription>密钥只写不读：GET 返回掩码，保存时留空或保留掩码表示不修改。</DialogDescription>
          </DialogHeader>
          {draft ? (
            <div className="grid gap-3 sm:grid-cols-2">
              <Label className="sm:col-span-2">Worker 地址<Input value={draft.worker_url} onChange={(event) => setDraft({ ...draft, worker_url: event.target.value })} /></Label>
              <Label className="sm:col-span-2">管理密钥<Input value={draft.admin_key} onChange={(event) => setDraft({ ...draft, admin_key: event.target.value })} placeholder="留空或保留掩码 = 不修改" /></Label>
              <Label>池 ID
                <select className="mt-1 h-9 w-full rounded-md border bg-transparent px-2 text-sm" value={draft.pool_id} onChange={(event) => setDraft({ ...draft, pool_id: event.target.value })}>
                  <option value="B">B（快池）</option>
                  <option value="A">A（慢池）</option>
                </select>
              </Label>
              <Label>容量<Input type="number" value={draft.capacity} onChange={(event) => setDraft({ ...draft, capacity: Number(event.target.value) || 0 })} /></Label>
              <Label>每日补充<Input type="number" value={draft.daily_grant} onChange={(event) => setDraft({ ...draft, daily_grant: Number(event.target.value) || 0 })} /></Label>
              <Label>安全垫（天）<Input type="number" value={draft.reserve_days} onChange={(event) => setDraft({ ...draft, reserve_days: Number(event.target.value) || 0 })} /></Label>
              <Label>目标账号数<Input type="number" value={draft.target} onChange={(event) => setDraft({ ...draft, target: Number(event.target.value) || 0 })} /></Label>
              <Label>推送间隔（分）<Input type="number" value={draft.push_minutes} onChange={(event) => setDraft({ ...draft, push_minutes: Number(event.target.value) || 0 })} /></Label>
              <Label>轮转（号 / 分）<Input type="number" value={draft.refresh_per_tick} onChange={(event) => setDraft({ ...draft, refresh_per_tick: Number(event.target.value) || 0 })} /></Label>
              <Label>轮转间隔（分）<Input type="number" value={draft.refresh_minutes} onChange={(event) => setDraft({ ...draft, refresh_minutes: Number(event.target.value) || 0 })} /></Label>
              <Label className="flex items-center gap-2">自动推送<input type="checkbox" checked={draft.auto_push} onChange={(event) => setDraft({ ...draft, auto_push: event.target.checked })} /></Label>
              <Label className="flex items-center gap-2">启用联动<input type="checkbox" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} /></Label>
              <Label className="sm:col-span-2">出口密钥
                <select className="mt-1 h-9 w-full rounded-md border bg-transparent px-2 text-sm" value={draft.key_id} onChange={(event) => setDraft({ ...draft, key_id: event.target.value })}>
                  <option value="">（不绑定）</option>
                  {(keysQuery.data?.items ?? []).map((key) => (
                    <option key={key.id} value={key.id}>{key.name} · {key.pool || "个人"}</option>
                  ))}
                </select>
              </Label>
            </div>
          ) : (
            <div className="flex h-32 items-center justify-center"><Spinner className="size-5" /></div>
          )}
          <DialogFooter>
            <Button variant="ghost" onClick={() => setCfgOpen(false)}>取消</Button>
            <Button onClick={() => draft && saveConfigMutation.mutate(draft)} disabled={saveConfigMutation.isPending}>保存</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
