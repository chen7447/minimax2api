// Registration sweep driver: one exit IP per account, cooldown between rounds,
// backoff on consecutive failures. Runs tools/login.mjs as a child per round.
//
//   node tools/sweep.mjs --mail-key KEY [--rounds 5] [--interval 300]
//                        [--backoff 900] [--consecutive-fail 2] [--round-timeout 540]
//                        [--verbose]
//
// Exit-IP rotation goes through Clash Verge's mihomo named pipe (the browser
// is launched with --proxy-server=http://127.0.0.1:7897, Verge's mixed port,
// so switching the "全部节点" selector group switches the browser's exit).
import { spawn, execFileSync } from "node:child_process";
import net from "node:net";
import fs from "node:fs";
import os from "node:os";

const args = {};
for (let i = 2; i < process.argv.length; i += 1) {
  const a = process.argv[i];
  const v = () => process.argv[++i];
  if (a === "--mail-key") args.mailKey = v();
  else if (a === "--mail-domain") args.mailDomain = v();
  else if (a === "--rounds") args.rounds = Number(v());
  else if (a === "--interval") args.interval = Number(v());
  else if (a === "--backoff") args.backoff = Number(v());
  else if (a === "--consecutive-fail") args.consecutiveFail = Number(v());
  else if (a === "--round-timeout") args.roundTimeout = Number(v());
  else if (a === "--pool") args.pool = v();
  else if (a === "--controller") args.controller = v();
  else if (a === "--exit-proxy") args.exitProxy = v();
  else if (a === "--headed") args.headed = true;
  else if (a === "--pin") args.pin = true;
  else if (a === "--patient") args.patient = true;
  else if (a === "--verbose") args.verbose = true;
  else if (a === "--group") args.group = v();
  else if (a === "--window-position") args.windowPosition = v();
  else if (a === "--window-size") args.windowSize = v();
  else if (a === "--until") args.until = v();
  else if (a === "--no-site-check") args.noSiteCheck = true;
  else if (a === "--site-check-tries") args.siteCheckTries = Number(v());
  else if (a === "--site-check-max-ms") args.siteCheckMaxMs = Number(v());
  else if (a === "--accounts-per-ip") args.accountsPerIp = Number(v());
  else if (a === "--recency-min") args.recencyMin = Number(v());
  else if (a === "--max-delay") args.maxDelay = Number(v());
  else if (a === "--no-delay-test") args.noDelayTest = true;
  else if (a === "--require-google") args.requireGoogle = true;
  else { console.log(`无法识别的参数：${a}`); process.exit(2); }
}
args.rounds ??= 5;
args.interval ??= 300; // seconds between accounts
args.backoff ??= 900; // seconds after consecutive failures
args.consecutiveFail ??= 2;
// One stuck CDP call must not hang the whole batch: kill the round when it
// outlives this budget (the login child's next round self-heals the profile).
args.roundTimeout ??= 540;
if (!args.mailKey) {
  for (const p of ["capture.local.json", "../capture.local.json", "F:/Minimax2api/capture.local.json"]) {
    try {
      if (fs.existsSync(p)) {
        const cfg = JSON.parse(fs.readFileSync(p, "utf8"));
        if (cfg.mailKey) { args.mailKey = cfg.mailKey; break; }
      }
    } catch {}
  }
}
if (!args.mailKey) { console.log("缺少 --mail-key"); process.exit(2); }

function parseUntil(val) {
  if (!val) return null;
  const match = String(val).match(/^(\d{1,2}):(\d{2})$/);
  if (match) {
    const target = new Date();
    target.setHours(Number(match[1]), Number(match[2]), 0, 0);
    if (target.getTime() <= Date.now()) {
      target.setDate(target.getDate() + 1);
    }
    return target.getTime();
  }
  const parsed = Date.parse(val);
  return isNaN(parsed) ? null : parsed;
}
const untilMs = parseUntil(args.until);
if (untilMs) {
  console.log(`[sweep] 已设置截止运行时间：${args.until}（预计运行至 ${new Date(untilMs).toLocaleString()}）`);
}

const PIPE = String.raw`\\.\pipe\verge-mihomo`;
const SECRET = "set-your-secret";
const GROUP = "全部节点";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const stamp = () => new Date().toTimeString().slice(0, 8);
const log = (m) => console.log(`[${stamp()}] ${m}`);

function pipeRequest(method, path, body = null, timeoutMs = 12000) {
  return new Promise((resolve, reject) => {
    const sock = net.connect({ path: PIPE });
    let buf = "";
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      const sep = buf.indexOf("\r\n\r\n");
      if (sep < 0) { reject(new Error("no headers")); return; }
      const status = Number((buf.match(/HTTP\/1\.[01] (\d+)/) || [])[1] || 0);
      let raw = buf.slice(sep + 4);
      const a = raw.indexOf("{");
      const b = raw.lastIndexOf("}");
      resolve({ status, body: a >= 0 && b > a ? raw.slice(a, b + 1) : raw });
    };
    const timer = setTimeout(() => { sock.destroy(); finish(); }, timeoutMs);
    sock.on("error", (e) => { if (!done) { done = true; clearTimeout(timer); reject(e); } });
    sock.on("data", (d) => (buf += d));
    sock.on("end", finish);
    sock.on("close", finish);
    sock.write(
      `${method} ${path} HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer ${SECRET}\r\n` +
      (body ? `Content-Type: application/json\r\nContent-Length: ${Buffer.byteLength(body)}\r\n` : "") +
      `Connection: close\r\n\r\n${body || ""}`,
    );
  });
}

// Transport: named-pipe REST for the shared Clash Verge (default), or plain
// TCP REST when --controller points at our dedicated reg-proxy instance.
async function apiRequest(method, apiPath, body = null) {
  if (args.controller) {
    const res = await fetch(args.controller + apiPath, {
      method,
      headers: { "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : null,
    });
    return { status: res.status, body: await res.text() };
  }
  return pipeRequest(method, apiPath, body);
}

let nodePool = null;
async function loadNodes() {
  if (nodePool) return nodePool;
  const p = await apiRequest("GET", "/proxies");
  const data = JSON.parse(p.body);
  nodePool = (data.proxies[GROUP]?.all || []).filter(
    (n) => !/剩余流量|套餐到期|DIRECT|REJECT/.test(n),
  );
  if (!nodePool.length) throw new Error(`分组 ${GROUP} 没有可用节点`);
  // Operator observation (2026-10-07): the post-registration OAuth redirect
  // is much faster on US exits — SG/JP exits stall there and burn the stall
  // window. Try US nodes first; the rest stay as fallback in pool order.
  nodePool = [
    ...nodePool.filter((n) => /US|美国|🇺🇸/i.test(n)),
    ...nodePool.filter((n) => !/US|美国|🇺🇸/i.test(n)),
  ];
  return nodePool;
}

async function selectNode(name) {
  await apiRequest("PUT", `/proxies/${encodeURIComponent(GROUP)}`, JSON.stringify({ name }));
  await sleep(1500);
}

function exitIp() {
  // two probes: some nodes serve ipify poorly; api64 is Cloudflare-backed and
  // answers over both stacks
  const proxy = args.exitProxy || "http://127.0.0.1:7897";
  for (const url of ["https://api.ipify.org", "https://api64.ipify.org"]) {
    try {
      const ip = execFileSync("curl.exe", ["-s", "-m", "6", "-x", proxy, url], { encoding: "utf8" }).trim();
      if (/^\d+\.\d+\.\d+\.\d+$/.test(ip)) return ip;
    } catch {}
  }
  return "";
}

/**
 * siteReachable fetches the real login page several times through the proxy and
 * reports whether the node can actually serve the SPA.
 *
 * A live exit IP is NOT enough. Measured on 2026-10-11: node US_7 answered
 * ipify and returned 200 for the login HTML, so the old IP-only probe waved it
 * through — then the page's ~99 asset requests (cdn.hailuo.ai) stalled, the SPA
 * never hydrated, and 6 rounds died on the 540s watchdog. A single fetch is
 * also not enough (US_7 gave 200 at 6.7s / 8.2s before failing). The decisive,
 * cheap signal is *consistency*: N consecutive 200s, each fast, and a low
 * median. Throughput is a red herring — US_11 took 21s for 1MB yet went 10/10,
 * while US_7 failed every fetch.
 */
function siteReachable() {
  const proxy = args.exitProxy || "http://127.0.0.1:7897";
  const tries = Number(args.siteCheckTries || 3);
  const maxMs = Number(args.siteCheckMaxMs || 8000);
  const times = [];
  for (let i = 0; i < tries; i += 1) {
    try {
      const out = execFileSync("curl.exe", [
        "-s", "-o", os.devNull, "-w", "%{http_code}|%{time_total}",
        "-m", String(Math.ceil(maxMs / 1000)), "-x", proxy, "https://agent.minimax.io/login",
      ], { encoding: "utf8" }).trim();
      const [code, t] = out.split("|");
      if (code !== "200") return { ok: false, why: `第 ${i + 1}/${tries} 次 HTTP ${code || "无响应"}` };
      times.push(Number(t) * 1000);
    } catch {
      return { ok: false, why: `第 ${i + 1}/${tries} 次超时/断连` };
    }
  }
  const sorted = [...times].sort((a, b) => a - b);
  const median = sorted[Math.floor(sorted.length / 2)];
  if (median > maxMs) return { ok: false, why: `中位延迟 ${Math.round(median)}ms > ${maxMs}ms` };
  return { ok: true, median: Math.round(median) };
}

// ---------------------------------------------------------- exit-IP rotation
//
// Ported from StepFun's clash.mjs/batch.mjs: each residential exit serves a few
// accounts (3~4) before rotating; the next node is chosen by (a) a live delay
// test, (b) never the node just used, (c) least-used-first so every IP gets the
// same share, (d) a recency window so an IP is not revisited too soon, and
// (e) a blacklist for nodes that keep failing the pre-check.
// Node pool, assigned by main(); the rotation helpers below read it.
let nodes = [];
// Current exit and how many accounts it has served in this sweep process.
let activeNode = null;
let activeNodeIp = "";
let activeNodeQuota = 0;
let activeNodeLimit = 0;

const NODE_STATE = "node-state.json";
const loadNodeState = () => {
  try { return { usage: {}, fails: {}, blacklist: [], ...JSON.parse(fs.readFileSync(NODE_STATE, "utf8")) }; }
  catch { return { usage: {}, fails: {}, blacklist: [] }; }
};
const saveNodeState = (s) => { try { fs.writeFileSync(NODE_STATE, JSON.stringify(s, null, 1)); } catch {} };

/** measureDelays: mihomo's own per-node delay test for the group. */
async function measureDelays() {
  if (args.noDelayTest) return {};
  const path = `/group/${encodeURIComponent(GROUP)}/delay?url=${encodeURIComponent("http://www.gstatic.com/generate_204")}&timeout=3000`;
  try {
    const res = await apiRequest("GET", path);
    return JSON.parse(res.body);
  } catch {
    return {};
  }
}

// Google reachability is what the reCAPTCHA gate actually needs: MiniMax's
// password step loads enterprise.js from www.google.com, and a node that cannot
// reach it can never finish a registration (measured 2026-10-19: 68/157 nodes
// reach Google). Cached — the concurrent delay test takes a few seconds.
let googleCache = { at: 0, data: {} };
async function measureGoogleDelays() {
  if (Date.now() - googleCache.at < 10 * 60000) return googleCache.data;
  const path = `/group/${encodeURIComponent(GROUP)}/delay?url=${encodeURIComponent("https://www.google.com/generate_204")}&timeout=5000`;
  try {
    const res = await apiRequest("GET", path);
    googleCache = { at: Date.now(), data: JSON.parse(res.body) };
  } catch {
    googleCache = { at: Date.now(), data: {} };
  }
  return googleCache.data;
}

/** recordBad marks a node as failed; two strikes blacklists it for the run. */
function recordBad(name) {
  const state = loadNodeState();
  state.fails[name] = (state.fails[name] || 0) + 1;
  if (state.fails[name] >= 2 && !state.blacklist.includes(name)) {
    state.blacklist.push(name);
    log(`⛔ 已拉黑节点（累计 2 次预检失败）：${name}`);
  }
  saveNodeState(state);
}

/**
 * pickNextNode chooses the next exit: delay-tested, not the current node, not
 * blacklisted, least-used first (even spread), then recency-preferred.
 */
async function pickNextNode(excluded) {
  const delays = await measureDelays();
  const state = loadNodeState();
  const now = Date.now();
  const recencyMs = Number(args.recencyMin || 45) * 60000;
  const maxDelay = Number(args.maxDelay || 2000);

  // When the reCAPTCHA gate is on, only exits that reach Google can finish a
  // registration, so restrict the pool to them.
  const google = args.requireGoogle ? await measureGoogleDelays() : null;
  // If the delay test itself failed (empty map) the filter would reject every
  // node and abort the whole batch — fall back to "no Google filter" instead.
  const googleUsable = Boolean(google) && Object.values(google).some((d) => Number(d) > 0);
  const base = nodes.filter((n) => !excluded.has(n) && n !== activeNode
    && !state.blacklist.includes(n)
    && (!googleUsable || Number(google[n]) > 0));
  if (!base.length) return null;
  const alive = base.filter((n) => {
    const d = Number(delays[n]);
    return d > 0 && d <= maxDelay;
  });
  const pool = alive.length ? alive : base; // if the delay test is empty, fall back to the raw pool
  const used = (n) => state.usage[n]?.count || 0;
  pool.sort((a, b) => used(a) - used(b) || (Number(delays[a]) || 9999) - (Number(delays[b]) || 9999));
  const minUsed = used(pool[0]);
  const tier = pool.filter((n) => used(n) === minUsed);
  const fresh = tier.filter((n) => {
    const last = state.usage[n]?.lastUsed || 0;
    return now - last >= recencyMs;
  });
  const from = fresh.length ? fresh : tier;
  return from[Math.floor(Math.random() * from.length)];
}

/**
 * activateNode picks + validates a new exit (live IP probe, then the login-page
 * pre-check), bumps its usage counter, and returns { name, ip }.
 */
async function activateNode() {
  const tried = new Set();
  for (let t = 0; t < 60; t += 1) {
    const candidate = await pickNextNode(tried);
    if (!candidate) return null;
    tried.add(candidate);
    await selectNode(candidate);
    const gotIp = exitIp();
    if (!gotIp) {
      log(`节点 ${candidate} 出口探测失败，换下一个`);
      recordBad(candidate);
      continue;
    }
    if (!args.noSiteCheck) {
      const probe = siteReachable();
      if (!probe.ok) {
        log(`节点 ${candidate} 站点预检失败（IP ${gotIp} · ${probe.why}），换下一个`);
        recordBad(candidate);
        continue;
      }
      if (args.verbose) log(`节点 ${candidate} 站点预检通过（中位 ${probe.median}ms）。`);
    }
    const state = loadNodeState();
    state.usage[candidate] = { count: (state.usage[candidate]?.count || 0) + 1, lastUsed: Date.now() };
    saveNodeState(state);
    return { name: candidate, ip: gotIp };
  }
  return null;
}

function runLogin(round, proxy) {
  return new Promise((resolve) => {
    const child = spawn(
      process.execPath,
      ["tools/login.mjs", "--new-mailbox", "--mail-key", args.mailKey, "--proxy", proxy, "--once",
        ...(args.mailDomain ? ["--mail-domain", args.mailDomain] : []),
        ...(args.group ? ["--group", args.group] : []),
        ...(args.windowPosition ? ["--window-position", args.windowPosition] : []),
        ...(args.windowSize ? ["--window-size", args.windowSize] : []),
        ...(args.headed ? ["--headed"] : []),
        ...(args.patient ? ["--patient"] : []),
        ...(args.verbose ? ["--verbose"] : [])],
      { stdio: ["ignore", "pipe", "pipe"] },
    );
    let out = "";
    child.stdout.on("data", (d) => { const s = d.toString(); out += s; if (args.verbose) process.stdout.write(s); });
    child.stderr.on("data", (d) => { out += d.toString(); });
    // Watchdog: a wedged round (hung evaluate, dead proxy) gets killed at the
    // budget and recorded as a failure instead of stalling the sweep forever.
    const timer = setTimeout(() => {
      log(`第 ${round} 轮超过 ${args.roundTimeout}s 无进展，看门狗终止。`);
      try {
        if (process.platform === "win32") {
          execFileSync("taskkill.exe", ["/pid", String(child.pid), "/T", "/F"], { stdio: "ignore" });
        } else {
          child.kill("SIGKILL");
        }
      } catch {}
    }, args.roundTimeout * 1000);
    child.on("exit", (code) => {
      clearTimeout(timer);
      resolve({ code: code ?? -1, out });
    });
  });
}

async function main() {
  // --pool mode: candidates come from a pre-probed free-proxy list (one fresh
  // exit IP per attempt); Clash rotation is bypassed entirely.
  const poolCandidates = args.pool
    ? JSON.parse(fs.readFileSync(args.pool, "utf8")).candidates
    : null;
  if (args.pool && !poolCandidates?.length) { log("候选池为空，中止。"); process.exit(2); }
  nodes = args.pool || args.pin ? [] : await loadNodes();
  log(args.pool
    ? `免费池候选 ${poolCandidates.length} 个（来源 ${args.pool}）`
    : args.pin
      ? `钉住手选出口（不轮换节点）· 浏览器代理 ${args.exitProxy || "http://127.0.0.1:7897"}`
      : `节点池 ${nodes.length} 个 · 控制端 ${args.controller || "Clash Verge 管道"} · 浏览器代理 ${args.exitProxy || "http://127.0.0.1:7897"}`);
  const results = [];
  let nodeCursor = 0;
  let fails = 0;
  for (let round = 1; round <= args.rounds; round += 1) {
    if (untilMs && Date.now() >= untilMs) {
      log(`已到达截止时间 ${args.until}，批次按计划圆满结束。`);
      break;
    }
    let node = null;
    let ip = "";
    let proxyUrl = args.exitProxy || "http://127.0.0.1:7897";
    if (args.pool) {
      node = poolCandidates.shift();
      if (!node) { log("候选池用尽，提前收工。"); break; }
      ip = node.ip;
      proxyUrl = node.proxy;
      // persist consumption so chunked/foreground runs never retry a used IP
      try { fs.writeFileSync(args.pool, JSON.stringify({ time: new Date().toISOString(), candidates: poolCandidates }, null, 1)); } catch {}
    } else if (args.pin) {
      // 手选出口实验：绝不 PUT 节点选择，钉住用户在 Verge 里选的出口
      node = "手选固定出口";
      ip = exitIp();
      log(`第 ${round}/${args.rounds} 轮 · 出口「${node}」· IP ${ip || "探测失败（照跑，注册会自己失败）"}`);
    } else {
      // One exit serves 3~4 accounts before rotating (StepFun's model): a single
      // residential IP registering back-to-back is what trips the rate limiter.
      if (!activeNode || activeNodeQuota >= activeNodeLimit) {
        const picked = await activateNode();
        if (!picked) { log("整个节点池都探测不到可用出口，中止。"); break; }
        activeNode = picked.name;
        activeNodeIp = picked.ip;
        activeNodeQuota = 0;
        activeNodeLimit = Number(args.accountsPerIp) > 0
          ? Number(args.accountsPerIp)
          : 3 + (Math.random() < 0.5 ? 0 : 1); // 3 or 4 accounts per exit
        log(`⇄ 切换出口「${activeNode}」· IP ${activeNodeIp} · 本节点计划 ${activeNodeLimit} 个号`);
      }
      activeNodeQuota += 1;
      node = activeNode;
      ip = activeNodeIp;
    }
    log(`第 ${round}/${args.rounds} 轮 · 出口「${node.proxy || node}」· IP ${ip}${node.country ? ` (${node.country})` : ""}`);

    const started = Date.now();
    const { code, out } = await runLogin(round, proxyUrl);
    const secs = Math.round((Date.now() - started) / 1000);
    const ok = code === 0;
    const email = (out.match(/已创建邮箱：(\S+@[^\s\(\)]+)/) || [])[1] || "?";
    const pw = (out.match(/使用密码：(\S+?)（/)|| [])[1] || "?";
    const dead = /判定为 IP 限流/.test(out);
    log(`第 ${round} 轮${ok ? "✅ 成功" : "❌ 失败"} · ${email} · ${secs}s${dead ? " · 滑块限流" : ""}${ok ? "" : ` · 尾部: ${out.trim().split("\n").slice(-2).join(" / ").slice(0, 180)}`}`);
    results.push({ round, node, ip, ok, secs, email, pw, dead });
    if (!ok) appendFailure(email, pw);
    // Failures attributable to the EXIT (page never rendered, connection
    // dropped, reCAPTCHA gate unreachable) are handled by switching IP, so they
    // must not trip the global backoff — that cooldown is for systemic problems.
    // The gate failure also proves the node can't reach Google regardless of
    // what the delay test said: count a strike and stop spending its quota.
    const nodeLevelFail = !ok && /reCAPTCHA 门禁脚本加载失败|未能在登录页找到邮箱输入框|ERR_CONNECTION_CLOSED|邮箱步滞留|找不到「Continue」按钮|登录步骤停滞|登录步骤超过 6 轮/.test(out);
    if (nodeLevelFail) {
      if (/reCAPTCHA 门禁脚本加载失败/.test(out)) recordBad(node);
      activeNodeQuota = activeNodeLimit;
    }
    // Keep every round's full output: the [cap-loop]/[cap-drag]/[cap-verdict]
    // lines are the landing forensics the success-rate analysis is built from.
    try {
      fs.appendFileSync("sweep-rounds.log",
        `\n===== ${new Date().toISOString()} · 第 ${round} 轮 · ${node?.proxy || node} · ${ok ? "OK" : "FAIL"} =====\n${out}\n`);
    } catch {}

    if (ok) fails = 0;
    else if (!nodeLevelFail) fails += 1;
    else log(`  本轮属出口级失败（换 IP 即可），不计入退避计数（连续失败仍为 ${fails}）`);
    if (round < args.rounds) {
      let wait = args.interval;
      if (fails >= args.consecutiveFail) {
        wait = args.backoff;
        log(`连败 ${fails} 次，加长冷却到 ${wait / 60} 分钟`);
        fails = 0;
      }
      if (untilMs && Date.now() + wait * 1000 >= untilMs) {
        log(`下一轮将超过截止时间 ${args.until}，批次按计划结束。`);
        break;
      }
      await sleep(wait * 1000);
    }
  }
  const okCount = results.filter((r) => r.ok).length;
  log(`==== 汇总：${okCount}/${results.length} 成功 ====`);
  for (const r of results) log(`  轮${r.round} ${r.ok ? "✅" : "❌"} ${r.node?.proxy || r.node} (${r.ip}) ${r.secs}s ${r.email}`);
}

function appendFailure(email, pw) {
  // failed rounds leave half-registered mailboxes; keep them so the password
  // re-login path can recover later
  try {
    fs.appendFileSync("sweep-failures.log", `${new Date().toISOString()}\t${email}\t${pw}\n`);
  } catch {}
}

main().catch((e) => { console.error(e); process.exit(2); });
