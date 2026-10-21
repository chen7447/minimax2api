#!/usr/bin/env node
/**
 * Local credential-capture helper.
 *
 * The console cannot read a MiniMax credential: the token lives in another
 * origin's storage, and no page may reach into it. So the console publishes a
 * capture request and this helper answers it from the operator's own machine,
 * where a real browser can log in.
 *
 * What it does, once per request:
 *   1. opens the site in Chrome under a dedicated profile (kept between runs,
 *      so the login survives and later captures are instant),
 *   2. waits for `_token` to appear in localStorage — i.e. for the operator to
 *      finish signing in,
 *   3. reads the browser's own fingerprint alongside it (`UNIQUE_USER_ID` and
 *      `tab_device_id`) and posts all three to the console,
 *   4. closes the browser and goes back to waiting.
 *
 * It also watches the session while it works: the console's force-cancel and
 * the server-side timeout both have to be able to stop a capture, so this loop
 * is never a blind wait for a credential that may never come.
 *
 * Usage:
 *   node tools/capture.mjs [--base URL] [--key KEY] [--config FILE]
 *                          [--profile DIR] [--port 9223] [--once] [--verbose]
 *
 * Config resolution: flags, then MINIMAX2API_BASE / MINIMAX2API_ADMIN_KEY, then
 * a JSON file at --config / $MINIMAX2API_CAPTURE_CONFIG / the repository's
 * parent directory (`capture.local.json`) / the current directory. The admin
 * key belongs in that file, which sits outside the repository on purpose.
 */

import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));

const DEFAULT_POLL_MS = 2000;
const WORK_POLL_MS = 1500;
const CDP_TIMEOUT_MS = 25_000;

/** The two deployments. A token from one is rejected by the other. */
const SITES = {
  cn: "https://agent.minimaxi.com",
  global: "https://agent.minimax.io",
};

/** Storage keys the site keeps the credential and its fingerprint in. */
const TOKEN_KEY = "_token";
const UUID_KEY = "UNIQUE_USER_ID";
const DEVICE_KEY = "tab_device_id";

const args = parseArgs(process.argv.slice(2));
const config = loadConfig(args);
const verbose = Boolean(args.verbose);

if (!config.base) fail("缺少服务地址：用 --base 指定，或写进配置文件。");
if (!config.key) fail(`缺少管理员密钥：用 --key 指定，或写进 ${config.source || "配置文件"}。`);

let browser = null; // The instance this run owns, so it can be closed again.

// ------------------------------------------------------------------- main loop

async function main() {
  log(`MiniMax2API 抓取助手已启动 · ${config.base}`);
  log(`浏览器配置目录：${config.profile}`);
  log(args.once ? "单次模式：取一个请求就退出" : "常驻模式：等待控制台下发抓取请求（Ctrl+C 退出）");

  for (;;) {
    let pending = null;
    try {
      pending = await pollPending();
    } catch (error) {
      // In one-shot mode an unreachable console is an answer, not something to
      // retry: the operator asked for a single attempt.
      if (args.once) fail(`无法连接控制台：${error.message}`);
      log(`无法连接控制台：${error.message}`);
      await sleep(DEFAULT_POLL_MS);
      continue;
    }

    if (!pending) {
      if (args.once) {
        log("当前没有待抓取的请求。");
        return;
      }
      await sleep(DEFAULT_POLL_MS);
      continue;
    }

    await capture(pending);
    if (args.once) return;
  }
}

/**
 * capture runs one request to completion: open, wait for the login, deliver,
 * close. Every exit path closes the browser, including cancel and timeout —
 * a helper that leaves a window behind would make the next capture open a
 * second one, and the operator would not know which to use.
 */
async function capture(request) {
  const site = SITES[request.region];
  if (!site) {
    log(`请求的区域无法识别：${request.region}`);
    return;
  }
  const deadline = Date.parse(request.expiresAt || "") || Date.now() + 180_000;
  log(`收到抓取请求（${request.region === "cn" ? "国内站" : "国际站"}）· ${site}`);

  try {
    await ensureBrowser();
    const page = await openSite(site);
    log("浏览器已打开，请在窗口里完成登录…");

    const credential = await waitForCredential(page, request, deadline);
    if (!credential) return; // Cancelled or timed out; waitForCredential logged why.

    await deliver(request.id, credential);
  } catch (error) {
    log(`抓取失败：${error.message}`);
  } finally {
    await closeBrowser();
  }
}

// ------------------------------------------------------------------ the wait

/**
 * waitForCredential polls the page until the token appears, and the session
 * until it stops being live. Whichever comes first wins.
 *
 * The session poll doubles as this helper's heartbeat: the console shows
 * "helper online" from the same request, so a long capture must keep polling or
 * the operator would be told the helper had died while it was working.
 */
async function waitForCredential(page, request, deadline) {
  let lastKeys = [];
  for (;;) {
    const credential = await readCredential(page);
    if (credential) return credential;

    let session = null;
    try {
      session = await pollPending();
    } catch {
      // A console that went away is not a reason to keep a browser open.
      log("与控制台失去联系，停止本次抓取。");
      return null;
    }
    if (!session) {
      log(`抓取已结束：${await describeEnd(request.id)}`);
      return null;
    }
    if (Date.now() > deadline + 5000) {
      log("已超过服务端的抓取时限，停止本次抓取。");
      return null;
    }

    // Log the storage keys once, and only the names: a capture that finds
    // nothing is almost always the upstream having renamed a key, and the names
    // are what make that visible. Values are credentials and stay out of the
    // log.
    if (verbose || Date.now() > deadline - 30_000) {
      const keys = await listStorageKeys(page);
      if (keys.join(",") !== lastKeys.join(",")) {
        lastKeys = keys;
        log(`页面当前可见的存储键：${keys.join(", ") || "（空）"}`);
      }
    }

    await sleep(WORK_POLL_MS);
  }
}

/** readCredential reads the credential and the fingerprint that goes with it. */
async function readCredential(page) {
  const expression = `JSON.stringify({
    token: localStorage.getItem(${JSON.stringify(TOKEN_KEY)}),
    uuid: localStorage.getItem(${JSON.stringify(UUID_KEY)}),
    device: sessionStorage.getItem(${JSON.stringify(DEVICE_KEY)}),
  })`;
  let raw;
  try {
    raw = await evaluate(page, expression);
  } catch {
    return null; // Mid-navigation; the next tick will read it.
  }
  if (!raw) return null;

  let values;
  try {
    values = JSON.parse(raw);
  } catch {
    return null;
  }
  const token = (values.token || "").trim();
  if (!token) return null;

  return {
    token,
    uuid: (values.uuid || "").trim(),
    deviceID: (values.device || "").trim(),
  };
}

async function listStorageKeys(page) {
  try {
    const raw = await evaluate(
      page,
      `JSON.stringify({
        local: Object.keys(localStorage),
        session: Object.keys(sessionStorage),
      })`,
    );
    const keys = JSON.parse(raw || "{}");
    return [...(keys.local || []), ...(keys.session || [])].slice(0, 40);
  } catch {
    return [];
  }
}

// ------------------------------------------------------------------- delivery

async function deliver(id, credential) {
  const response = await request(`/admin/api/capture/${id}/credential`, {
    method: "POST",
    body: {
      token: credential.token,
      uuid: credential.uuid,
      deviceID: credential.deviceID,
    },
  });
  const name = response?.account?.name || "账号";
  log(response?.created ? `已添加账号 ${name}` : `已更新账号 ${name}`);
}

/** describeEnd asks the console why the session is no longer live. */
async function describeEnd(id) {
  try {
    const view = await request("/admin/api/capture");
    const session = view?.capture;
    if (!session || session.id !== id) return "会话已被新的请求替换";
    if (session.state === "cancelled") return "已被强行取消";
    if (session.state === "expired") return "已超时";
    if (session.state === "delivered") return "凭证已入库";
    return session.state;
  } catch {
    return "状态未知";
  }
}

// --------------------------------------------------------------------- chrome

async function ensureBrowser() {
  if (await debuggerUp()) {
    log("复用已打开的浏览器实例。");
    browser = { port: config.port, reused: true };
    return;
  }

  const executable = findChrome();
  if (!executable) {
    throw new Error("找不到 Chrome，请用 CHROME_PATH 指定浏览器可执行文件");
  }
  fs.mkdirSync(config.profile, { recursive: true });

  const child = spawn(
    executable,
    [
      `--remote-debugging-port=${config.port}`,
      `--user-data-dir=${config.profile}`,
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-features=Translate",
      "about:blank",
    ],
    { detached: true, stdio: "ignore" },
  );
  child.unref();
  browser = { port: config.port, reused: false };

  const started = Date.now();
  while (Date.now() - started < CDP_TIMEOUT_MS) {
    if (await debuggerUp()) return;
    await sleep(300);
  }
  throw new Error("浏览器启动了但调试端口没有监听");
}

/** openSite creates a tab on the site and returns its CDP session. */
async function openSite(site) {
  const res = await fetch(`http://127.0.0.1:${config.port}/json/new?${encodeURIComponent(site)}`, {
    method: "PUT",
  });
  if (!res.ok) throw new Error(`无法打开页面：HTTP ${res.status}`);
  const target = await res.json();
  const page = await attach(target.webSocketDebuggerUrl);
  await page.send("Runtime.enable");
  await page.send("Page.enable");
  return page;
}

/**
 * closeBrowser shuts the whole instance down, not just the tab: this profile
 * exists only for captures, so leaving it running would leave a stray window on
 * the operator's desktop.
 */
async function closeBrowser() {
  if (!browser) return;
  try {
    const version = await fetchJson(`http://127.0.0.1:${config.port}/json/version`);
    if (version?.webSocketDebuggerUrl) {
      const connection = await attach(version.webSocketDebuggerUrl);
      await connection.send("Browser.close").catch(() => {});
      connection.close();
    }
  } catch {
    // Already gone is the expected outcome, not a failure.
  }
  browser = null;
}

async function debuggerUp() {
  try {
    const version = await fetchJson(`http://127.0.0.1:${config.port}/json/version`);
    return Boolean(version?.webSocketDebuggerUrl);
  } catch {
    return false;
  }
}

function findChrome() {
  const candidates = [
    process.env.CHROME_PATH,
    "C:/Program Files/Google/Chrome/Application/chrome.exe",
    "C:/Program Files (x86)/Google/Chrome/Application/chrome.exe",
    "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/usr/bin/google-chrome",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
  ].filter(Boolean);
  return candidates.find((candidate) => fs.existsSync(candidate)) || null;
}

// ------------------------------------------------------------------ cdp client

async function attach(wsUrl) {
  const ws = new WebSocket(wsUrl);
  await new Promise((resolve, reject) => {
    ws.addEventListener("open", resolve, { once: true });
    ws.addEventListener("error", () => reject(new Error("无法连接浏览器的调试端口")), { once: true });
  });

  let nextId = 0;
  const pending = new Map();
  ws.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    if (message.id === undefined || !pending.has(message.id)) return;
    const { resolve, reject } = pending.get(message.id);
    pending.delete(message.id);
    if (message.error) reject(new Error(message.error.message || "CDP 调用失败"));
    else resolve(message.result);
  });

  return {
    send(method, params = {}) {
      const id = ++nextId;
      return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        ws.send(JSON.stringify({ id, method, params }));
      });
    },
    close() {
      ws.close();
    },
  };
}

async function evaluate(page, expression) {
  const { result, exceptionDetails } = await page.send("Runtime.evaluate", {
    expression,
    returnByValue: true,
    awaitPromise: true,
  });
  if (exceptionDetails) throw new Error(exceptionDetails.text || "页面求值失败");
  return result.value;
}

// ----------------------------------------------------------------- http calls

/**
 * pollPending asks for work and, by asking, reports that this helper is alive.
 * A null answer means "nothing to do" — either no request was made, or the one
 * being worked on has ended.
 */
async function pollPending() {
  const body = await request("/admin/api/capture/pending");
  return body?.capture ?? null;
}

async function request(pathname, options = {}) {
  const headers = { authorization: `Bearer ${config.key}` };
  if (options.body !== undefined) headers["content-type"] = "application/json";
  const response = await fetch(`${config.base}${pathname}`, {
    method: options.method || "GET",
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  });
  const text = await response.text();
  if (!response.ok) {
    let message = `HTTP ${response.status}`;
    try {
      message = JSON.parse(text).error || message;
    } catch {
      // Keep the status line; the body is HTML or empty.
    }
    throw new Error(message);
  }
  // A console that does not know this route answers with its SPA shell and a
  // 200 — the app's catch-all serves index.html for any unknown GET. Parsed
  // loosely, that reads as "no capture requested" and the helper would sit
  // there looking healthy against a console that cannot capture at all.
  try {
    return text ? JSON.parse(text) : null;
  } catch {
    throw new Error("控制台返回的不是 JSON（版本过旧，或地址写错了）");
  }
}

async function fetchJson(url) {
  const response = await fetch(url);
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  return response.json();
}

// ------------------------------------------------------------------ utilities

function parseArgs(argv) {
  const parsed = { once: false, verbose: false };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const value = () => argv[++i];
    if (arg === "--base") parsed.base = value();
    else if (arg === "--key") parsed.key = value();
    else if (arg === "--config") parsed.config = value();
    else if (arg === "--profile") parsed.profile = value();
    else if (arg === "--port") parsed.port = Number(value());
    else if (arg === "--once") parsed.once = true;
    else if (arg === "--verbose") parsed.verbose = true;
    else if (arg === "--help" || arg === "-h") {
      console.log(
        [
          "用法：node tools/capture.mjs [选项]",
          "",
          "  --base URL      控制台地址（默认读配置文件 / MINIMAX2API_BASE）",
          "  --key KEY       管理员密钥（默认读配置文件 / MINIMAX2API_ADMIN_KEY）",
          "  --config FILE   配置文件路径（默认 capture.local.json，在仓库上级目录或当前目录）",
          "  --profile DIR   Chrome 配置目录（默认 %LOCALAPPDATA%\\minimax2api-capture-profile）",
          "  --port N        调试端口（默认 9223）",
          "  --once          只处理一个请求就退出",
          "  --verbose       打印页面里的存储键名（排查上游改名用）",
        ].join("\n"),
      );
      process.exit(0);
    } else {
      fail(`无法识别的参数：${arg}`);
    }
  }
  return parsed;
}

/**
 * loadConfig merges the flags, the environment and a JSON file, in that order
 * of precedence. The file is where the admin key belongs: it lives outside the
 * repository, next to the other local secrets.
 */
function loadConfig(parsed) {
  const file = findConfigFile(parsed);
  let stored = {};
  if (file) {
    try {
      stored = JSON.parse(fs.readFileSync(file, "utf8"));
    } catch (error) {
      fail(`配置文件无法解析：${file}（${error.message}）`);
    }
  }
  const port = Number(parsed.port || stored.port || 9223);
  return {
    base: (parsed.base || process.env.MINIMAX2API_BASE || stored.base || "").replace(/\/$/, ""),
    key: parsed.key || process.env.MINIMAX2API_ADMIN_KEY || stored.key || "",
    profile: parsed.profile || stored.profile || defaultProfile(),
    port: Number.isFinite(port) ? port : 9223,
    source: file || "",
  };
}

function findConfigFile(parsed) {
  const candidates = [
    parsed.config,
    process.env.MINIMAX2API_CAPTURE_CONFIG,
    // The repository's parent directory: F:\Minimax2api\capture.local.json
    // sits next to the other local secrets, outside version control.
    path.join(HERE, "..", "..", "capture.local.json"),
    path.join(process.cwd(), "capture.local.json"),
  ].filter(Boolean);
  return candidates.find((candidate) => fs.existsSync(candidate)) || "";
}

function defaultProfile() {
  const root = process.platform === "win32"
    ? process.env.LOCALAPPDATA || path.join(os.homedir(), "AppData", "Local")
    : path.join(os.homedir(), ".cache");
  return path.join(root, "minimax2api-capture-profile");
}

function log(message) {
  console.log(`[${new Date().toTimeString().slice(0, 8)}] ${message}`);
}

function fail(message) {
  console.error(message);
  process.exit(1);
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Ctrl+C must not leave the browser behind: the profile is dedicated, and a
// stray window would confuse the next capture.
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    void closeBrowser().finally(() => process.exit(0));
  });
}

main()
  .then(() => process.exit(0))
  .catch((error) => {
    console.error("抓取助手异常退出：", error);
    process.exit(1);
  });
