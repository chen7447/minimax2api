#!/usr/bin/env node
// One-shot rescue: reopen the register profile whose session already passed
// the slider / sits at the email-code step, finish the registration and
// deliver the credential. Usage:
//   node tools/rescue.mjs --mail-key KEY --admin-key KEY [--base URL]
//                         [--proxy URL] [--port N]
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const AGENT_ORIGIN = "https://agent.minimax.io";
const ACCOUNT_ORIGIN = "https://account.minimax.io";
const MAIL_BASE = "https://mail.7447.site";
const MAIL_KEY = (() => {
  const i = process.argv.indexOf("--mail-key");
  return i > 0 ? process.argv[i + 1] : "";
})();
// The console admin key is never hardcoded here: it comes from --admin-key or
// MINIMAX2API_ADMIN_KEY, and the console address from --base.
const ADMIN_KEY = (() => {
  const i = process.argv.indexOf("--admin-key");
  return i > 0 ? process.argv[i + 1] : process.env.MINIMAX2API_ADMIN_KEY || "";
})();
const ADMIN_BASE = (() => {
  const i = process.argv.indexOf("--base");
  return (i > 0 ? process.argv[i + 1] : process.env.MINIMAX2API_BASE) || "https://minimax2api.7447.site";
})();
const PROXY = (() => {
  const i = process.argv.indexOf("--proxy");
  return i > 0 ? process.argv[i + 1] : process.env.LOGIN_PROXY || "";
})();
const PORT = (() => {
  const i = process.argv.indexOf("--port");
  return i > 0 ? Number(process.argv[i + 1]) : 9228;
})();
const PROFILE = path.join(process.env.LOCALAPPDATA || "", "minimax2api-register");
const log = (m) => console.log(`[${new Date().toTimeString().slice(0, 8)}] ${m}`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function fail(m) { console.error(m); process.exit(1); }
if (!MAIL_KEY) fail("缺少 --mail-key");

function findChrome() {
  return [
    process.env.CHROME_PATH,
    "C:/Program Files/Google/Chrome/Application/chrome.exe",
    "C:/Program Files (x86)/Google/Chrome/Application/chrome.exe",
  ].find((c) => c && fs.existsSync(c));
}

async function debuggerUp() {
  try {
    const r = await fetch(`http://127.0.0.1:${PORT}/json/version`);
    return Boolean((await r.json())?.webSocketDebuggerUrl);
  } catch { return false; }
}

async function attach(wsUrl) {
  const ws = new WebSocket(wsUrl);
  await new Promise((resolve, reject) => {
    ws.addEventListener("open", resolve, { once: true });
    ws.addEventListener("error", () => reject(new Error("无法连接调试端口")), { once: true });
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
    close() { ws.close(); },
  };
}

async function main() {
  if (!(await debuggerUp())) {
    const exe = findChrome();
    if (!exe) fail("找不到 Chrome");
    const child = spawn(exe, [
      `--remote-debugging-port=${PORT}`,
      `--user-data-dir=${PROFILE}`,
      "--no-first-run", "--no-default-browser-check",
      "--disable-blink-features=AutomationControlled",
      ...(PROXY ? [`--proxy-server=${PROXY}`] : []),
      "about:blank",
    ], { detached: true, stdio: "ignore" });
    child.unref();
    const started = Date.now();
    while (Date.now() - started < 20000 && !(await debuggerUp())) await sleep(300);
    if (!(await debuggerUp())) fail("浏览器起了但调试端口没监听");
    log("已用原 profile 重开浏览器（会话未动）");
  } else {
    log("复用已在运行的浏览器实例");
  }

  const version = await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json();
  const pages = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
  const pageTarget = pages.find((t) => t.type === "page");
  if (!pageTarget?.webSocketDebuggerUrl) fail("没有可附加的页面 target");
  const page = await attach(pageTarget.webSocketDebuggerUrl);
  await page.send("Runtime.enable").catch(() => {});
  await page.send("Page.enable").catch(() => {});
  await page.send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false }).catch(() => {});

  const evaluate = async (expression) => {
    const { result, exceptionDetails } = await page.send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true });
    if (exceptionDetails) throw new Error(exceptionDetails.text || "求值失败");
    return result.value;
  };
  const clickAt = async (x, y) => {
    await page.send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y });
    await page.send("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", clickCount: 1 });
    await page.send("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", clickCount: 1 });
  };

  // 回到 agent 起点并进 OAuth（同 login.mjs 的入口构造）
  await page.send("Page.navigate", { url: AGENT_ORIGIN + "/" }).catch(() => {});
  await sleep(5000);
  if (!(await evaluate(`Boolean(localStorage.getItem("_token"))`).catch(() => false))) {
    const oauthEntry = ACCOUNT_ORIGIN + "/oauth2/authorize?client_id=agent-minimax" +
      "&redirect_uri=" + encodeURIComponent(AGENT_ORIGIN + "/auth/callback") +
      "&response_type=code&source=agent_web&state=" +
      encodeURIComponent(Buffer.from(JSON.stringify({ redirect_uri: AGENT_ORIGIN + "/", csrf: crypto.randomUUID() })).toString("base64url"));
    await page.send("Page.navigate", { url: oauthEntry }).catch(() => {});
    await sleep(6000);
  }

  // 观察当前落点：验证码步骤（会显示邮箱）还是别的
  let mailbox = null;
  for (let i = 0; i < 20; i += 1) {
    const state = await evaluate(`(() => {
      const t = document.body.innerText || "";
      const m = t.match(/verification code to\\s+(\\S+@\\S+)/i);
      const emailInput = Boolean(document.querySelector('input[type="email"]'));
      const codeInput = [...document.querySelectorAll("input")].some(el => el.type !== "email" && el.type !== "password" && /code|验证码/i.test(el.placeholder || "") || (el.type !== "email" && el.type !== "password" && t.includes("verification code")));
      return JSON.stringify({ url: location.href.slice(0, 90), hasCodeText: /verification code/i.test(t), email: m ? m[1] : null, emailInput, codeInput, head: t.slice(0, 120).replace(/\\n+/g, " | ") });
    })()`).catch(() => null);
    if (!state) { await sleep(1500); continue; }
    log(`页面状态: ${state.head}（url ${state.url}）`);
    if (state.email) { mailbox = state.email; log(`会话仍在验证码步骤，邮箱 = ${mailbox}`); break; }
    if (state.hasCodeText && state.codeInput) { log("在验证码步骤但没解析出邮箱"); break; }
    // 停在统一登录首页：尝试把 email 输入框里已有的值读出来
    const prefilled = await evaluate(`(() => { const el = document.querySelector('input[type="email"]'); return el ? el.value : ""; })()`).catch(() => "");
    if (prefilled && /@7447\.site$/.test(prefilled)) { mailbox = prefilled; log(`邮箱输入框里已有值 = ${mailbox}，提交继续`); await evaluate(`(() => { const el = document.querySelector('input[type="email"]'); el && el.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`).catch(() => {}); const c = await evaluate(`(() => { const el = document.querySelector('input[type="email"]'); if (!el) return null; const r = el.getBoundingClientRect(); return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 }); })()`); await clickAt(JSON.parse(c).x, JSON.parse(c).y); await sleep(4000); continue; }
    await sleep(2500);
  }
  if (!mailbox) fail("没能定位到邮箱（页面不在验证码步骤）。未做任何破坏性操作，profile 原样保留。");

  // 从邮箱 API 拿最新验证码（读不到就点 Resend 再等）
  let code = null;
  for (let round = 0; round < 6 && !code; round += 1) {
    const url = `${MAIL_BASE}/v1/messages?address=${encodeURIComponent(mailbox)}&limit=10`;
    const response = await fetch(url, { headers: { "X-API-Key": MAIL_KEY } });
    const data = await response.json().catch(() => null);
    const messages = data?.data?.messages || [];
    for (const message of messages) {
      const detail = await fetch(`${MAIL_BASE}/v1/messages/${message.id}?address=${encodeURIComponent(mailbox)}`, { headers: { "X-API-Key": MAIL_KEY } }).then((r) => r.json()).catch(() => null);
      const raw = detail?.data?.raw || "";
      const body = raw.split(/\r?\n\r?\n/).slice(1).join("\n\n") || raw;
      const plain = body.replace(/<[^>]+>/g, " ").replace(/=\r?\n/g, "");
      const m = plain.match(/code[^0-9]{0,40}(\d{6})|(\d{6})[^0-9]{0,40}code/i) || plain.match(/\b(\d{6})\b/);
      if (m) { code = m[1] || m[2]; break; }
    }
    if (!code) {
      log("邮箱里没有可用验证码，点一次 Resend…");
      await evaluate(`(() => { const btn = [...document.querySelectorAll("button")].find(b => /resend|重新发送/i.test(b.innerText) && !b.disabled); if (btn) { btn.click(); return true; } return false; })()`).catch(() => {});
      await sleep(12000);
    }
  }
  if (!code) fail("拿不到验证码（含 Resend 重试）。profile 原样保留，可重跑本脚本。");
  log(`验证码已取得：${code}`);

  // 输入并提交
  await evaluate(`(() => {
    const inputs = [...document.querySelectorAll("input")].filter(el => el.type !== "email" && el.type !== "password");
    if (!inputs.length) throw new Error("找不到验证码输入框");
    const el = inputs[0];
    el.focus();
    return true;
  })()`).catch((e) => fail(e.message));
  const box = await evaluate(`(() => { const inputs = [...document.querySelectorAll("input")].filter(el => el.type !== "email" && el.type !== "password"); const r = inputs[0].getBoundingClientRect(); return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 }); })()`);
  const { x, y } = JSON.parse(box);
  await clickAt(x, y);
  await clickAt(x, y); await clickAt(x, y); // 三连击全选
  await page.send("Input.insertText", { text: code });
  await sleep(800);
  // 勾协议（未勾时）+ Continue
  await evaluate(`(() => { const box = [...document.querySelectorAll("button")].find(b => b.className.includes("size-4")); if (box && !box.querySelector("svg")) { const r = box.getBoundingClientRect(); return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 }); } return null; })()`).then((c) => c && clickAt(JSON.parse(c).x, JSON.parse(c).y)).catch(() => {});
  await evaluate(`(() => {
    const btn = [...document.querySelectorAll("button")].find(b => /^continue$/i.test(b.innerText.trim()) && !b.disabled);
    if (!btn) return null;
    const r = btn.getBoundingClientRect();
    return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
  })()`).then((c) => c && clickAt(JSON.parse(c).x, JSON.parse(c).y)).catch(() => {});
  log("验证码已提交，等待回跳与 token…");

  // 等 token
  let credential = null;
  for (let i = 0; i < 90; i += 1) {
    const raw = await evaluate(`JSON.stringify({ token: localStorage.getItem("_token"), uuid: localStorage.getItem("UNIQUE_USER_ID"), detail: JSON.parse(localStorage.getItem("user_detail_agent") || "null") })`).catch(() => null);
    if (raw) {
      const parsed = JSON.parse(raw);
      if (parsed.token) {
        try {
          const [, payload] = parsed.token.split(".");
          const claims = JSON.parse(atob(payload.replace(/-/g, "+").replace(/_/g, "/")));
          if (claims?.user?.id && parsed.detail?.realUserID) { credential = parsed; break; }
        } catch {}
      }
    }
    await sleep(1000);
  }
  if (!credential?.token) fail("等了 90s 没拿到带 user.id 的 token。");
  log(`token 已取得（长度 ${credential.token.length}，uuid ${credential.uuid || "无"}）`);

  const record = { time: new Date().toISOString(), email: mailbox, password: "(round5-未知,会话续用)", token: credential.token, uuid: credential.uuid || null, userId: credential.detail?.realUserID != null ? String(credential.detail.realUserID) : null, userName: credential.detail?.userName || null };
  fs.appendFileSync(path.join(HERE, "..", "..", "credentials-local.jsonl"), JSON.stringify(record) + "\n");
  log("凭证已落盘 credentials-local.jsonl");
  const payload = { token: credential.token, uuid: credential.uuid, region: "global", identifier: credential.detail?.userName || mailbox, group: "login-pipeline", remark: `tools/rescue.mjs 抢救（${mailbox}）` };
  if (credential.detail?.realUserID) payload.userId = String(credential.detail.realUserID);
  const res = await fetch(`${ADMIN_BASE}/admin/api/accounts`, { method: "POST", headers: { authorization: `Bearer ${ADMIN_KEY}`, "content-type": "application/json" }, body: JSON.stringify(payload) });
  log("入库：HTTP", res.status, (await res.text()).slice(0, 160));
}

main().then(() => process.exit(0)).catch((e) => { console.error("抢救异常：", e?.message || e); process.exit(1); });
