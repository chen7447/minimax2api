import { useTranslation } from "react-i18next";
import { Navigate, useParams } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { PageHeader } from "@/shared/components/page-header";
import { cn } from "@/shared/lib/cn";

type EndpointDoc = {
  category: string;
  endpoint: string;
  method: "GET" | "POST";
  path: string;
  titleKey: string;
  descriptionKey: string;
  request: string;
  response: string;
  // Every parameter note is a key, including the ones that read like prose:
  // this page is the API reference, and an operator switching the console to
  // English should not find half of it in Chinese.
  parameters: Array<{ name: string; type: string; required?: boolean; noteKey: string }>;
};

const ENDPOINTS: EndpointDoc[] = [
  {
    category: "chat",
    endpoint: "completions",
    method: "POST",
    path: "/v1/chat/completions",
    titleKey: "docs.chatCompletions",
    descriptionKey: "docs.chatDescription",
    request: `curl http://127.0.0.1:8080/v1/chat/completions \\
  -H "Authorization: Bearer sk-mm-xxxx" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "minimax-m3-thinking",
    "stream": true,
    "messages": [
      { "role": "user", "content": "用一句话解释快速排序" }
    ]
  }'`,
    response: `data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"delta":{"content":"快速排序"}}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"delta":{"reasoning_content":"先选基准值"}}]}

data: [DONE]`,
    parameters: [
      { name: "model", type: "string", required: true, noteKey: "docs.noteModel" },
      { name: "messages", type: "array", required: true, noteKey: "docs.noteMessages" },
      { name: "stream", type: "boolean", noteKey: "docs.noteStream" },
      { name: "temperature", type: "number", noteKey: "docs.notePassthrough" },
      { name: "max_tokens", type: "number", noteKey: "docs.noteMaxTokens" },
    ],
  },
  {
    category: "chat",
    endpoint: "models",
    method: "GET",
    path: "/v1/models",
    titleKey: "docs.modelList",
    descriptionKey: "docs.overview",
    request: `curl http://127.0.0.1:8080/v1/models \\
  -H "Authorization: Bearer sk-mm-xxxx"`,
    response: `{
  "object": "list",
  "data": [
    { "id": "minimax-agent", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-m3.1-flash", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-m3.1-flash-preview", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-m3", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-m3-thinking", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-m2.7", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-m2.7-highspeed", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-image", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-h3", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-h3-max", "object": "model", "owned_by": "minimax" },
    { "id": "minimax-hailuo-2-3", "object": "model", "owned_by": "minimax" }
  ]
}`,
    parameters: [],
  },
  {
    category: "image",
    endpoint: "generations",
    method: "POST",
    path: "/v1/images/generations",
    titleKey: "docs.imageGenerations",
    descriptionKey: "docs.imageDescription",
    request: `curl http://127.0.0.1:8080/v1/images/generations \\
  -H "Authorization: Bearer sk-mm-xxxx" \\
  -H "Content-Type: application/json" \\
  -d '{
    "prompt": "一只在花园里散步的猫，写实风格",
    "response_format": "url"
  }'`,
    response: `{
  "created": 1792667805,
  "status": "succeeded",
  "data": [
    {
      "url": "/media/media_10acc8ff.png",
      "source_url": "https://matrix-internal.oss-us-east-1.aliyuncs.com/..."
    }
  ]
}

// 没出图时：Agent 的原话在 detail 里
{ "created": 1792667805, "status": "pending", "detail": "……", "data": [] }

// 轮次失败：保活已开始，HTTP 仍是 200
{ "created": 1792667805, "status": "failed", "detail": "……", "data": [] }`,
    parameters: [
      { name: "prompt", type: "string", required: true, noteKey: "docs.notePrompt" },
      { name: "response_format", type: "string", noteKey: "docs.noteResponseFormat" },
      { name: "n", type: "number", noteKey: "docs.noteN" },
      { name: "size", type: "string", noteKey: "docs.noteSize" },
    ],
  },
  {
    category: "video",
    endpoint: "generations",
    method: "POST",
    path: "/v1/videos/generations",
    titleKey: "docs.videoGenerations",
    descriptionKey: "docs.videoDescription",
    request: `curl http://127.0.0.1:8080/v1/videos/generations \\
  -H "Authorization: Bearer sk-mm-xxxx" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "minimax-h3-max",
    "prompt": "a paper boat drifting on a calm pond, gentle ripples",
    "duration": 5,
    "ratio": "16:9",
    "resolution": "768P"
  }'`,
    response: `{
  "created": 1792666573,
  "model": "minimax-h3-max",
  "status": "succeeded",
  "data": [
    {
      "url": "/media/media_07b50cd7.mp4",
      "source_url": "https://matrix-internal.oss-us-east-1.aliyuncs.com/..."
    }
  ]
}

// 超时前还没出片（H3.0 的常态）：任务已提交，detail 是 Agent 的原话
{
  "created": 1792666573,
  "model": "minimax-h3",
  "status": "pending",
  "detail": "……",
  "data": []
}

// 轮次失败：保活已开始，HTTP 仍是 200，审计里仍记 502
{
  "created": 1792666573,
  "model": "minimax-h3-max",
  "status": "failed",
  "detail": "……",
  "data": []
}`,
    parameters: [
      { name: "prompt", type: "string", required: true, noteKey: "docs.noteVideoPrompt" },
      { name: "model", type: "string", noteKey: "docs.noteVideoModel" },
      { name: "duration", type: "number", noteKey: "docs.noteDuration" },
      { name: "ratio", type: "string", noteKey: "docs.noteRatio" },
      { name: "resolution", type: "string", noteKey: "docs.noteResolution" },
      { name: "image_url", type: "string", noteKey: "docs.noteImageURL" },
      { name: "image_urls", type: "array", noteKey: "docs.noteImageURLs" },
    ],
  },
  {
    category: "system",
    endpoint: "health",
    method: "GET",
    path: "/health",
    titleKey: "docs.health",
    descriptionKey: "docs.healthDescription",
    request: `curl http://127.0.0.1:8080/health`,
    response: `{
  "status": "ok",
  "version": "0.1.0",
  "pool": { "total": 12, "available": 10, "cooldown": 1, "invalid": 1 }
}`,
    parameters: [],
  },
];

export function ApiDocsPage() {
  const { t } = useTranslation();
  const params = useParams<{ category: string; endpoint: string }>();
  const doc = ENDPOINTS.find((item) => item.category === params.category && item.endpoint === params.endpoint);

  // A route that resolves to nothing sends the caller to a real endpoint rather
  // than quietly rendering the first one. Falling back would answer a URL like
  // /docs/image/generations with the chat page — a wrong answer that looks like
  // an answer, and the one failure mode nobody reports because nothing looks
  // broken.
  if (!doc) {
    const first = ENDPOINTS[0];
    return <Navigate to={`/docs/${first.category}/${first.endpoint}`} replace />;
  }
  const base = typeof window === "undefined" ? "http://127.0.0.1:8080" : window.location.origin;

  return (
    <div className="space-y-5">
      <PageHeader title={t("docs.title")} description={t("docs.description")} />

      <section className="rounded-lg bg-card p-4">
        <h2 className="text-xs font-medium">{t("docs.overview")}</h2>
        <dl className="mt-3 grid gap-4 sm:grid-cols-2">
          <div>
            <dt className="text-[10px] text-muted-foreground">{t("docs.baseURL")}</dt>
            <dd className="mt-1 font-mono text-xs">{base}</dd>
          </div>
          <div>
            <dt className="text-[10px] text-muted-foreground">{t("docs.auth")}</dt>
            <dd className="mt-1 font-mono text-xs">Authorization: Bearer &lt;client key&gt;</dd>
          </div>
        </dl>
        <p className="mt-3 text-[11px] leading-5 text-muted-foreground">{t("docs.authHelp")}</p>
      </section>

      <section className="rounded-lg bg-card p-4">
        <h2 className="text-xs font-medium">{t("docs.integration")}</h2>
        <ul className="mt-3 space-y-2 text-[11px] leading-5 text-muted-foreground">
          <li>{t("docs.integrationDirect")}</li>
          <li>{t("docs.integrationNewApi")}</li>
          <li>{t("docs.integrationModels")}</li>
          <li>{t("docs.integrationVideo")}</li>
          <li>{t("docs.integrationErrors")}</li>
        </ul>
        <h2 className="mt-4 text-xs font-medium">{t("docs.creditTitle")}</h2>
        <p className="mt-2 text-[11px] leading-5 text-muted-foreground">{t("docs.creditBody")}</p>
      </section>

      <section className="rounded-lg bg-card p-4">
        <header className="flex flex-wrap items-center gap-2">
          <Badge
            variant="outline"
            className={cn(
              "font-mono",
              doc.method === "GET" ? "text-emerald-600 dark:text-emerald-400" : "text-sky-600 dark:text-sky-400",
            )}
          >
            {doc.method}
          </Badge>
          <code className="font-mono text-xs">{doc.path}</code>
          <span className="text-xs text-muted-foreground">· {t(doc.titleKey)}</span>
        </header>
        <p className="mt-3 text-xs leading-6 text-muted-foreground">{t(doc.descriptionKey)}</p>

        {doc.parameters.length > 0 ? (
          <div className="mt-4">
            <h3 className="text-xs font-medium">{t("docs.parameters")}</h3>
            <div className="mt-2 overflow-x-auto">
              <table className="w-full text-xs">
                <thead>
                  <tr className="border-b text-muted-foreground">
                    <th className="h-8 px-2 text-left font-normal">{t("docs.parameter")}</th>
                    <th className="h-8 px-2 text-left font-normal">{t("docs.parameterType")}</th>
                    <th className="h-8 px-2 text-left font-normal">{t("docs.required")}</th>
                    <th className="h-8 px-2 text-left font-normal">{t("docs.parameterNote")}</th>
                  </tr>
                </thead>
                <tbody>
                  {doc.parameters.map((parameter) => (
                    <tr key={parameter.name} className="border-b last:border-0">
                      <td className="px-2 py-2 font-mono">{parameter.name}</td>
                      <td className="px-2 py-2 text-muted-foreground">{parameter.type}</td>
                      <td className="px-2 py-2 text-muted-foreground">{parameter.required ? t("docs.required") : "—"}</td>
                      <td className="px-2 py-2 text-muted-foreground">{parameter.noteKey ? t(parameter.noteKey) : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        ) : null}

        <div className="mt-4 grid gap-3 lg:grid-cols-2">
          <div className="space-y-1">
            <h3 className="text-xs font-medium">{t("docs.requestExample")}</h3>
            <pre className="max-h-72 overflow-auto rounded-md bg-secondary/60 p-3 font-mono text-[11px] leading-5">{doc.request}</pre>
          </div>
          <div className="space-y-1">
            <h3 className="text-xs font-medium">{t("docs.responseExample")}</h3>
            <pre className="max-h-72 overflow-auto rounded-md bg-secondary/60 p-3 font-mono text-[11px] leading-5">{doc.response}</pre>
          </div>
        </div>
      </section>
    </div>
  );
}
