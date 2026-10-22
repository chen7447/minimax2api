import { useQuery } from "@tanstack/react-query";
import { Copy, Download, ImageOff, RefreshCw } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { apiRequest } from "@/shared/api/client";
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
  const galleryQuery = useQuery({
    queryKey: ["gallery"],
    queryFn: () => apiRequest<{ items: MediaItem[] }>("/admin/api/gallery"),
    refetchInterval: 15_000,
  });

  const items = galleryQuery.data?.items ?? [];
  const images = items.filter((item) => item.kind === "image");
  const videos = items.filter((item) => item.kind === "video");
  const visible = kind === "image" ? images : kind === "video" ? videos : items;

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
        <Tabs value={kind} onValueChange={setKind}>
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
        <Button variant="outline" size="sm" onClick={() => void galleryQuery.refetch()} disabled={galleryQuery.isFetching}>
          <RefreshCw className={cn(galleryQuery.isFetching && "animate-spin")} />
          {t("common.refresh")}
        </Button>
      </div>

      {galleryQuery.isPending ? (
        <div className="flex h-64 items-center justify-center">
          <Spinner className="size-5" />
        </div>
      ) : visible.length === 0 ? (
        <EmptyState label={items.length === 0 ? t("gallery.empty") : t("gallery.emptyFiltered")} />
      ) : (
        <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {visible.map((item) => (
            <article key={item.id} className="overflow-hidden rounded-lg bg-card">
              <div className={cn("relative bg-secondary/60", item.kind === "image" ? "aspect-square" : "aspect-video")}>
                {item.kind === "image" ? (
                  <img src={item.url} alt={item.prompt} loading="lazy" className="size-full object-cover" />
                ) : (
                  <video src={item.url} controls preload="metadata" className="size-full bg-black object-contain" />
                )}
                <Badge variant="secondary" className="absolute left-2 top-2 bg-background/85 backdrop-blur">
                  {item.kind === "image" ? t("models.typeImage") : t("models.typeVideo")}
                </Badge>
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
                  <Button variant="ghost" size="icon" className="h-7 w-7" onClick={() => void copyLink(item)} aria-label={t("gallery.copyLink")}>
                    <Copy />
                  </Button>
                </div>
                <p className="truncate font-mono text-[10px] text-muted-foreground">{item.model}</p>
                <p className="truncate text-[10px] text-muted-foreground">
                  {item.accountName || "—"} · {formatDateTime(item.createdAt, i18n.language)}
                </p>
              </div>
            </article>
          ))}
        </div>
      )}

      {items.length === 0 && !galleryQuery.isPending ? (
        <div className="flex items-center justify-center gap-2 text-xs text-muted-foreground">
          <ImageOff className="size-3.5" />
          {t("gallery.emptyHint")}
        </div>
      ) : null}
    </div>
  );
}
