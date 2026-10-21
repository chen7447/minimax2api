import { useMutation, useQuery } from "@tanstack/react-query";
import { ExternalLink, Globe } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { errorMessage } from "@/shared/api/client";

import {
  cancelCapture,
  getCapture,
  startCapture,
  type AccountRegion,
  type CaptureSession,
} from "./accounts-api";

/**
 * The two deployments the upstream runs. They share a product but not an
 * account database, and a token from one is rejected by the other, so the site
 * is a required choice rather than a preference.
 */
const SITES: Record<AccountRegion, { host: string; url: string }> = {
  cn: { host: "agent.minimaxi.com", url: "https://agent.minimaxi.com" },
  global: { host: "agent.minimax.io", url: "https://agent.minimax.io" },
};

/**
 * The command that starts the helper on the operator's own machine. Shown
 * verbatim, because the console cannot start it: the helper is a local process
 * and the console is served from a server that has no browser.
 */
const HELPER_COMMAND = "Minimax2api.bat capture";

/**
 * CaptureDialog drives a browser credential capture.
 *
 * The browser runs on the operator's machine, not here: the credential lives in
 * another origin's storage and no page can read it. So the console publishes a
 * request and a local helper answers it. Everything this dialog shows is
 * therefore a *state being polled*, not a call it is awaiting — which is also
 * why the force-cancel button is always available: an operator whose helper
 * died must be able to end the attempt without waiting for a timeout.
 */
export function CaptureDialog({
  open,
  onOpenChange,
  onCaptured,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCaptured: (session: CaptureSession) => void;
}) {
  const { t } = useTranslation();
  const [region, setRegion] = useState<AccountRegion>("global");
  // Which session this dialog is showing. Held locally so a session started
  // from another tab cannot make this one claim a result it did not ask for.
  const [sessionID, setSessionID] = useState("");
  // Delivery is reported once per session: the poll keeps returning the same
  // "delivered" state, and the callback refreshes the account list and toasts.
  const reported = useRef("");

  const captureQuery = useQuery({
    queryKey: ["capture"],
    queryFn: getCapture,
    enabled: open,
    // Poll only while something is actually in flight. A finished session is a
    // stable answer, and polling it forever would keep the console awake for no
    // reason.
    refetchInterval: (query) => (query.state.data?.capture.state === "waiting" ? 1500 : false),
  });
  const session = captureQuery.data?.capture;
  // A session is only shown if *this* dialog started it. The server keeps the
  // last session around so a delivery can be reported, and without this the
  // dialog would greet the operator with a stale "已取消" from a capture they
  // finished with long ago — which reads as something having just gone wrong.
  const watching = Boolean(session?.id) && session?.id === sessionID;
  const state = watching ? (session?.state ?? "idle") : "idle";
  const helperOnline = session?.helperOnline ?? false;

  useEffect(() => {
    if (!open || state !== "delivered" || !session || reported.current === session.id) return;
    reported.current = session.id;
    onCaptured(session);
    toast.success(
      session.updated
        ? t("accounts.captureUpdated", { name: session.account })
        : t("accounts.captureDelivered", { name: session.account }),
    );
  }, [open, state, session, onCaptured, t]);

  const startMutation = useMutation({
    mutationFn: () => startCapture(region),
    onSuccess: (result) => {
      setSessionID(result.capture.id);
      reported.current = "";
      void captureQuery.refetch();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const cancelMutation = useMutation({
    mutationFn: (id: string) => cancelCapture(id),
    onSuccess: () => void captureQuery.refetch(),
    onError: (error) => toast.error(errorMessage(error)),
  });

  const waiting = state === "waiting";
  const activeID = sessionID || session?.id || "";

  function close(next: boolean): void {
    // Closing mid-capture abandons it. Leaving the request standing would let a
    // helper that comes online later open a browser for a dialog nobody is
    // looking at — the same stuck state the cancel button exists to prevent.
    if (!next && waiting && activeID) cancelMutation.mutate(activeID);
    if (!next) setSessionID("");
    onOpenChange(next);
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("accounts.captureTitle")}</DialogTitle>
          <DialogDescription>{t("accounts.captureDescription")}</DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="capture-region">{t("accounts.captureSite")}</Label>
            <Select
              ariaLabel={t("accounts.captureSite")}
              value={region}
              disabled={waiting}
              onChange={(value) => setRegion(value as AccountRegion)}
              options={[
                { value: "cn", label: `${t("accounts.regionCN")} · ${SITES.cn.host}` },
                { value: "global", label: `${t("accounts.regionGlobal")} · ${SITES.global.host}` },
              ]}
            />
            <p className="text-[11px] leading-5 text-muted-foreground">{t("accounts.captureSiteHelp")}</p>
          </div>

          <div className="space-y-2 rounded-md border border-border/60 p-3">
            <div className="flex items-center gap-2">
              <StatusBadge state={state} />
              {waiting ? <Spinner className="size-3.5" /> : null}
              <span className="ml-auto text-[11px] text-muted-foreground">
                {helperOnline ? t("accounts.captureHelperOnline") : t("accounts.captureHelperOffline")}
              </span>
            </div>

            <p className="text-[11px] leading-5 text-muted-foreground">
              {state === "waiting"
                ? helperOnline
                  ? t("accounts.captureWaitingHelper")
                  : t("accounts.captureHelperMissing", { command: HELPER_COMMAND })
                : null}
              {state === "delivered" ? t("accounts.captureDeliveredHelp") : null}
              {state === "cancelled" ? t("accounts.captureCancelledHelp") : null}
              {state === "expired" ? t("accounts.captureExpiredHelp") : null}
              {state === "idle" ? t("accounts.captureIdleHelp", { command: HELPER_COMMAND }) : null}
            </p>

            {/* Logging in by hand first is worthwhile even with the helper
                offline: the capture then completes the moment it starts. */}
            <a
              className="inline-flex items-center gap-1.5 text-[11px] text-primary hover:underline"
              href={SITES[region].url}
              target="_blank"
              rel="noreferrer"
            >
              <Globe className="size-3.5" />
              {t("accounts.captureOpenSite", { host: SITES[region].host })}
              <ExternalLink className="size-3" />
            </a>
          </div>
        </div>

        <DialogFooter>
          {/* Always rendered while capturing, and never disabled: this is the
              escape hatch for a helper that hung, which is exactly when the
              other controls look stuck. */}
          {waiting ? (
            <Button
              variant="destructive"
              size="sm"
              disabled={!activeID || cancelMutation.isPending}
              onClick={() => activeID && cancelMutation.mutate(activeID)}
            >
              {t("accounts.captureForceCancel")}
            </Button>
          ) : (
            <Button variant="secondary" size="sm" onClick={() => close(false)}>
              {t("common.close")}
            </Button>
          )}
          {state === "delivered" || state === "cancelled" || state === "expired" ? (
            <Button size="sm" disabled={startMutation.isPending} onClick={() => startMutation.mutate()}>
              {t("accounts.captureAgain")}
            </Button>
          ) : (
            <Button
              size="sm"
              disabled={waiting || startMutation.isPending}
              onClick={() => startMutation.mutate()}
            >
              {t("accounts.captureStart")}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

const STATE_TONE: Record<string, "default" | "secondary" | "outline" | "destructive"> = {
  idle: "outline",
  waiting: "secondary",
  delivered: "default",
  cancelled: "outline",
  expired: "destructive",
};

function StatusBadge({ state }: { state: string }) {
  const { t } = useTranslation();
  return <Badge variant={STATE_TONE[state] ?? "outline"}>{t(`accounts.captureState.${state}`)}</Badge>;
}
