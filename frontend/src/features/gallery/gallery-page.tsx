import { useMutation, useQuery } from "@tanstack/react-query";
import { Copy, Download, ImageOff, RefreshCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { apiRequest, errorMessage } from "@/shared/api/client";
import { EmptyState } from "@/shared/components/data-state";
import { PageHeader } from "@/shared/components/page-header";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

type MediaItem = {
  id: string;
  kind: "image" | "video";
  url: string;
  sourceUrl?: string;
  prompt: string;
  model: string;
  accountName: string;
  createdAt: string;
};

// The stored url is a gateway path ("/media/media_xxx.mp4"). Downloading and
// copying both need an absolute one; the browser resolves it against the
// console's own origin, which is the same host that serves /media.
function absolute(url: string): string {
  if (/^https?:\/\//i.test(url)) return url;
  return new URL(url, window.location.origin).toString();
}

// Extension comes from the stored url, not from `kind`: the upstream has
// produced both .png and .jpg for images, and a download named .png that is
// really a jpeg is the kind of thing that only shows up when someone opens it.
function downloadName(item: MediaItem): string {
  const match = /\.([a-z0-9]+)(?:[?#]|$)/i.exec(item.url);
  const extension = match ? match[1].toLowerCase() : item.kind === "video" ? "mp4" : "png";
  return `${item.id}.${extension}`;
}

export function GalleryPage() {
  const { t, i18n } = useTranslation();
  const [kind, setKind] = useState("all");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [deleteTarget, setDeleteTarget] = useState<string[] | null>(null);

  const galleryQuery = useQuery({
    queryKey: ["gallery"],
    queryFn: () => apiRequest<{ items: MediaItem[] }>("/admin/api/gallery"),
    refetchInterval: 15_000,
  });

  const deleteMutation = useMutation({
    mutationFn: (ids: string[]) =>
      ids.length === 1
        ? apiRequest(`/admin/api/gallery/${ids[0]}`, { method: "DELETE" })
        : apiRequest("/admin/api/gallery/delete", { method: "POST", body: { ids } }),
    onSuccess: (_data, ids) => {
      toast.success(t("gallery.deleted", { count: ids.length }));
      setSelected(new Set());
      setDeleteTarget(null);
      void galleryQuery.refetch();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const items = galleryQuery.data?.items ?? [];
  const images = items.filter((item) => item.kind === "image");
  const videos = items.filter((item) => item.kind === "video");
  const visible = kind === "image" ? images : kind === "video" ? videos : items;

  // Selection is kept per visible list: switching the tab drops it, so a bulk
  // delete can never reach a card the operator is not looking at.
  const allSelected = visible.length > 0 && visible.every((item) => selected.has(item.id));
  const someSelected = visible.some((item) => selected.has(item.id));

  function toggle(id: string): void {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function toggleAll(): void {
    setSelected((current) => {
      if (allSelected) return new Set();
      const next = new Set(current);
      for (const item of visible) next.add(item.id);
      return next;
    });
  }

  async function copyLink(item: MediaItem): Promise<void> {
    try {
      await navigator.clipboard.writeText(absolute(item.url));
      toast.success(t("gallery.copied"));
    } catch {
      toast.error(t("errors.generic"));
    }
  }

  return (
    <div className="space-y-5">
      <PageHeader title={t("nav.gallery")} description={t("gallery.description")} />

      <div className="flex flex-wrap items-center justify-between gap-3">
        <Tabs
          value={kind}
          onValueChange={(value) => {
            setKind(value);
            setSelected(new Set());
          }}
        >
          <TabsList>
            <TabsTrigger value="all">
              {t("gallery.all")} · {items.length}
            </TabsTrigger>
            <TabsTrigger value="image">
              {t("gallery.images")} · {images.length}
            </TabsTrigger>
            <TabsTrigger value="video">
              {t("gallery.videos")} · {videos.length}
            </TabsTrigger>
          </TabsList>
        </Tabs>

        <div className="flex flex-wrap items-center gap-2">
          {visible.length > 0 ? (
            <label className="flex cursor-pointer items-center gap-1.5 text-xs text-muted-foreground">
              <Checkbox checked={allSelected ? true : someSelected ? "indeterminate" : false} onCheckedChange={toggleAll} />
              {t("gallery.selectAll")}
            </label>
          ) : null}
          <Button
            variant="destructive"
            size="sm"
            disabled={selected.size === 0 || deleteMutation.isPending}
            onClick={() => setDeleteTarget([...selected])}
          >
            <Trash2 />
            {t("gallery.deleteSelected", { count: selected.size })}
          </Button>
          <Button variant="outline" size="sm" onClick={() => void galleryQuery.refetch()} disabled={galleryQuery.isFetching}>
            <RefreshCw className={cn(galleryQuery.isFetching && "animate-spin")} />
            {t("common.refresh")}
          </Button>
        </div>
      </div>

      {galleryQuery.isPending ? (
        <div className="flex h-64 items-center justify-center">
          <Spinner className="size-5" />
        </div>
      ) : visible.length === 0 ? (
        <EmptyState label={items.length === 0 ? t("gallery.empty") : t("gallery.emptyFiltered")} />
      ) : (
        <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {visible.map((item) => {
            const isSelected = selected.has(item.id);
            return (
              <article
                key={item.id}
                className={cn("overflow-hidden rounded-lg bg-card", isSelected && "ring-1 ring-primary")}
              >
                <div className={cn("relative bg-secondary/60", item.kind === "image" ? "aspect-square" : "aspect-video")}>
                  {item.kind === "image" ? (
                    <img src={item.url} alt={item.prompt} loading="lazy" className="size-full object-cover" />
                  ) : (
                    <video src={item.url} controls preload="metadata" className="size-full bg-black object-contain" />
                  )}
                  <Badge variant="secondary" className="absolute left-2 top-2 bg-background/85 backdrop-blur">
                    {item.kind === "image" ? t("models.typeImage") : t("models.typeVideo")}
                  </Badge>
                  <span className="absolute right-2 top-2 rounded bg-background/85 p-1 backdrop-blur">
                    <Checkbox
                      checked={isSelected}
                      onCheckedChange={() => toggle(item.id)}
                      aria-label={t("gallery.selectAll")}
                    />
                  </span>
                </div>
                <div className="space-y-2 p-3">
                  <p className="line-clamp-2 min-h-8 text-[11px] leading-4 text-muted-foreground" title={item.prompt}>
                    {item.prompt || "—"}
                  </p>
                  <div className="flex items-center gap-1.5">
                    <Button asChild variant="outline" size="sm" className="h-7 flex-1 px-2">
                      <a href={item.url} download={downloadName(item)}>
                        <Download />
                        {t("gallery.download")}
                      </a>
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-7 w-7"
                      onClick={() => void copyLink(item)}
                      aria-label={t("gallery.copyLink")}
                    >
                      <Copy />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-7 w-7 text-muted-foreground hover:text-destructive"
                      onClick={() => setDeleteTarget([item.id])}
                      aria-label={t("gallery.delete")}
                    >
                      <Trash2 />
                    </Button>
                  </div>
                  <p className="truncate font-mono text-[10px] text-muted-foreground">{item.model}</p>
                  <p className="truncate text-[10px] text-muted-foreground">
                    {item.accountName || "—"} · {formatDateTime(item.createdAt, i18n.language)}
                  </p>
                </div>
              </article>
            );
          })}
        </div>
      )}

      {items.length === 0 && !galleryQuery.isPending ? (
        <div className="flex items-center justify-center gap-2 text-xs text-muted-foreground">
          <ImageOff className="size-3.5" />
          {t("gallery.emptyHint")}
        </div>
      ) : null}

      <AlertDialog open={deleteTarget !== null} onOpenChange={(open) => (!open ? setDeleteTarget(null) : undefined)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("gallery.deleteTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && deleteTarget.length > 1
                ? t("gallery.deleteMany", { count: deleteTarget.length })
                : t("gallery.deleteOne")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              disabled={deleteMutation.isPending}
              onClick={() => deleteTarget && deleteMutation.mutate(deleteTarget)}
            >
              {t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
