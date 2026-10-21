#!/usr/bin/env node
/**
 * Reproducible international-site login pipeline.
 *
 * capture.mjs harvests a credential from whatever session the operator already
 * has. This one creates the session itself: it drives a real Chrome through
 * account.minimax.io's unified login with an email + password (or an email
 * verification code fetched from the mail API), then delivers the resulting
 * token to the console — one command from "fresh mailbox" to "account in pool".
 *
 * Why a browser and not raw HTTP: the login page encrypts the password client-
 * side before it ever leaves the page, so the only client that can produce a
 * valid request is the page itself. Driving it through CDP costs nothing and
 * stays correct across frontend updates that would silently break a re-implementation.
 *
 * Flow, once per run:
 *   1. opens agent.minimax.io in a dedicated profile (login survives between
 *      runs, so re-harvesting is instant),
 *   2. clicks through Sign in → unified-login, types the email,
 *   3. answers whatever comes next — a password prompt, or a verification-code
 *      prompt whose code is polled from the mail API,
 *   4. waits for `_token` to appear in localStorage on the agent origin,
 *   5. reads the fingerprint beside it and POSTs everything to the console.
 *
 * Usage:
 *   node tools/login.mjs --email ADDR --password PW [--mail-key KEY]
 *                        [--base URL] [--key KEY] [--config FILE]
 *                        [--profile DIR] [--port N] [--no-deliver] [--verbose]
 *
 * With --mail-key the tool can also create the mailbox first (--new-mailbox),
 * and it always uses that key to fetch verification codes when the site asks
 * for one. Config resolution mirrors capture.mjs: flags, then environment,
 * then capture.local.json next to the repository.
 */

import { spawn, execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));

const AGENT_ORIGIN = "https://agent.minimax.io";
const ACCOUNT_ORIGIN = "https://account.minimax.io";
const MAIL_BASE = "https://mail.7447.site";

const NAV_TIMEOUT_MS = 30_000;
const STEP_POLL_MS = 500;
const STEP_TIMEOUT_MS = 25_000;
const CODE_POLL_MS = 4_000;
const CODE_TIMEOUT_MS = 300_000; // the send sometimes lands minutes late

const TOKEN_KEY = "_token";
const UUID_KEY = "UNIQUE_USER_ID";
const DETAIL_KEY = "user_detail_agent";

// Headless stamps "HeadlessChrome" into the UA — an instant captcha reject.
// The real version number is learned from /json/version at startup and spliced
// into a normal Windows Chrome UA.
let browserMajorVersion = 0;

function stealthUA() {
  const major = browserMajorVersion || 138;
  return `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${major}.0.0.0 Safari/537.36`;
}

const args = parseArgs(process.argv.slice(2));
const config = loadConfig(args);
const verbose = Boolean(args.verbose);

const required = [];
if (!config.email && !args.newMailbox) required.push("--email");
if (!config.password && !args.newMailbox) required.push("--password");
if (required.length) fail(`缺少参数：${required.join(" ")}`);
// A fresh registration needs a password too; the mailbox local part keeps the
// pipeline one-flag simple, mirroring how the operator names test accounts.
if (!config.password && args.newMailbox) config.password = config.email.split("@")[0];
if (!config.base && !args.noDeliver) fail("缺少服务地址：用 --base 指定，或写进配置文件。");

// ------------------------------------------------------------------- main flow

async function main() {
  log(`MiniMax2API 登录管线 · ${config.email}`);

  if (args.newMailbox) {
    // A registration must start logged out: the register profile is throwaway,
    // so reset it (after closing any instance holding it) instead of trying to
    // sign the previous account out through the UI.
    await closeBrowser();
    await killDebuggerOwner();
    fs.rmSync(config.profile, { recursive: true, force: true });
    const box = await createMailbox();
    config.email = box.address;
    config.mailProvider = box.provider;
    config.mailboxId = box.mailboxId || null;
    // The create-password step grades strength client-side and hides its
    // submit for weak values, so the mailbox local part (all lowercase) is not
    // a usable default; generate one strong enough and surface it in the log.
    if (!config.password) config.password = `Mm2api!${crypto.randomBytes(5).toString("hex")}`;
    log(`已创建邮箱：${box.address}`);
    log(`使用密码：${config.password}（请留底）`);
    ledgerUpdate(config.email, {
      stage: "registering",
      password: config.password,
      mailboxCreatedAt: new Date().toISOString(),
    });
    log("账号已落盘（阶段：已建号，密码在档）");
  }

  await ensureBrowser();
  const page = await openPage(AGENT_ORIGIN + "/");
  log("已打开 agent.minimax.io，开始登录流程…");

  const alreadyIn = await clickSignIn(page);
  if (!alreadyIn) {
    await typeEmail(page);
    await submitFirstStep(page);
    await runLoginSteps(page);
  } else {
    log("profile 里已有登录会话，直接取凭证。");
  }

  const credential = await waitForCredentialReady(page);
  if (!credential?.token) fail("登录流程走完但没有取到 token。");

  log(`token 已取得（长度 ${credential.token.length}，uuid ${credential.uuid || "（无）"}）`);
  log(`账号识别：${credential.detail?.userName || config.email} · realUserID ${credential.detail?.realUserID || "?"}`);

  // 落盘先于入库：浏览器 profile 每轮清空，控制台入库若失败，token 只存在于
  // 日志流里。先写本地 credentials-local.jsonl（git 仓库外语义，进 .gitignore），
  // 再尝试 deliver，失败也不丢凭证。
  persistCredential(credential);

  if (args.noDeliver) {
    log("--no-deliver：不入库，仅输出凭证摘要。");
  } else {
    await deliver(credential);
  }

  if (args.once) return;
}

/** clickSignInButton keeps hunting for the Sign in button until it lands. */
async function clickSignInButton(page) {
  const deadline = Date.now() + NAV_TIMEOUT_MS;
  while (Date.now() < deadline) {
    // The SPA hydrates asynchronously: a returning session may only surface
    // its token seconds after load, so keep re-checking while hunting for the
    // button.
    if (await readToken(page)) return true;
    const clicked = await evaluate(page, `(() => {
      const btns = [...document.querySelectorAll("button")].filter(b => /sign in/i.test(b.innerText));
      const hero = btns.find(b => b.type === "submit") || btns[0];
      if (hero) { hero.click(); return true; }
      return false;
    })()`).catch(() => false);
    if (clicked) break;
    await sleep(STEP_POLL_MS);
  }
}

/**
 * clickSignIn presses the hero "Sign in" button on the agent home, which
 * redirects to account.minimax.io's unified login with the OAuth state that
 * later binds the session back to the agent origin. Returns true when no sign-in
 * is needed because the profile already holds a live session.
 */
async function clickSignIn(page) {
  if (await readToken(page)) return true; // signed-in JWT already in storage
  // The home page builds the unified-login redirect client-side: a base64
  // state JSON whose csrf is just a random uuid. Fabricating it lets us enter
  // the OAuth flow directly — no slow home page, no ambiguous Sign-in buttons.
  const oauthEntry = ACCOUNT_ORIGIN + "/oauth2/authorize?client_id=agent-minimax" +
    "&redirect_uri=" + encodeURIComponent(AGENT_ORIGIN + "/auth/callback") +
    "&response_type=code&source=agent_web&state=" +
    encodeURIComponent(Buffer.from(JSON.stringify({
      redirect_uri: AGENT_ORIGIN + "/",
      csrf: crypto.randomUUID(),
    })).toString("base64url"));
  await page.send("Page.navigate", { url: oauthEntry }).catch(() => {});
  await sleep(3000);
  // The unified-login page renders an email input; wait for it to be certain
  // the redirect completed. The proxy makes hydration occasionally slow, so
  // one full reload-and-retry is cheaper than failing the run.
  for (let try_ = 0; try_ < 2; try_ += 1) {
    const ok = await waitForAny(page, [
      { name: "email-input", test: () => hasSelector(page, 'input[type="email"]') },
    ], STEP_TIMEOUT_MS * 1.6);
    if (ok === "email-input") {
      log("已进入统一登录页。");
      return false;
    }
    if (try_ === 0) {
      log("登录页 40 秒没渲染出来，整页重试…");
      await page.send("Page.navigate", { url: AGENT_ORIGIN + "/login" }).catch(() => {});
      await sleep(6000);
    }
  }
  throw new Error("未能在登录页找到邮箱输入框。");
}

async function typeEmail(page) {
  await typeIntoInput(page, 'input[type="email"]', config.email);
  if (verbose) log(`邮箱输入后页面文案：${(await pageText(page)).slice(0, 160).replace(/\n/g, " ")}`);
}

/** inputCoords centers an element in the viewport and returns its click point. */
async function inputCoords(page, selector) {
  const coords = await evaluate(page, `(() => {
    const el = document.querySelector(${JSON.stringify(selector)});
    if (!el) return null;
    el.scrollIntoView({ block: "center" });
    const r = el.getBoundingClientRect();
    return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
  })()`);
  if (!coords) fail(`找不到输入框 ${selector}`);
  return JSON.parse(coords);
}

/** typeIntoInput triple-clicks the field (selecting old content) and types. */
async function typeIntoInput(page, selector, text) {
  await clearAndType(page, await inputCoords(page, selector), text);
}

/** submitFirstStep ticks the agreement checkbox and presses Continue. */
async function submitFirstStep(page) {
  await clickAgreement(page);
  await clickButtonByText(page, "Continue");
}

/**
 * runLoginSteps walks whatever sequence the login page presents. A sign-in
 * asks for the password; a fresh registration may ask create-password →
 * verify-email-code → more. Each iteration answers one step and re-observes,
 * until the token shows up.
 */
async function runLoginSteps(page) {
  const stepPassword = () => hasSelector(page, 'input[type="password"]');
  const stepEmail = () => hasSelector(page, 'input[type="email"]');
  const stepCode = async () =>
    (await hasSelector(page, 'input:not([type="email"]):not([type="password"])')) &&
    /verification code|验证码/i.test(await pageText(page));
  for (let guard = 0; guard < 6; guard += 1) {
    const state = await waitForAny(page, [
      { name: "email", test: stepEmail },
      { name: "password", test: stepPassword },
      { name: "code", test: stepCode },
      { name: "loggedin", test: () => readToken(page).then(Boolean) },
    ]);
    if (verbose) {
      const text = (await pageText(page)).slice(0, 300).replace(/\n+/g, " | ");
      const probe = await evaluate(page, `JSON.stringify({
        buttons: [...document.querySelectorAll("button")].map(b => ({
          t: (b.innerText || "").trim().slice(0, 20) || "[空]",
          d: b.disabled,
          c: b.className.slice(0, 26),
        })),
        pwLens: [...document.querySelectorAll('input[type="password"]')].map(i => i.value.length),
        captcha: {
          turnstile: typeof window.turnstile,
          grecaptcha: typeof window.grecaptcha,
          hcaptcha: typeof window.hcaptcha,
          iframes: [...document.querySelectorAll("iframe")].map(f => {
            const r = f.getBoundingClientRect();
            return { src: (f.src || "").slice(0, 70), x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) };
          }),
        },
      })`).catch(() => "{}");
      log(`[verbose] 状态=${state} · 页面：${text}`);
      log(`[verbose] 探针：${probe}`);
    }
    if (state === "loggedin") {
      log("登录完成。");
      return;
    }
    if (!state) {
      // The post-code OAuth round trip crosses several redirects, and on a
      // slow proxy exit it can keep loading for minutes. While the wire is
      // still moving, waiting is correct — only a quiet wire AND no step AND
      // no token means the flow is truly stuck.
      const stallDeadline = Date.now() + 150_000;
      let late = null;
      let extended = 0;
      while (Date.now() < stallDeadline) {
        late = await waitForAny(page, [
          { name: "loggedin", test: () => readToken(page).then(Boolean) },
          { name: "email", test: stepEmail },
          { name: "password", test: stepPassword },
          { name: "code", test: stepCode },
        ], 10_000);
        if (late) break;
        if (netBusy(15_000)) {
          extended += 10;
          log(`页面暂无新步骤但网络仍在传输（慢出口加载中），已延长等待 ${extended}s…`);
          continue;
        }
        break;
      }
      if (late === "loggedin") {
        log("登录完成。");
        return;
      }
      const where = await evaluate(page, "location.href").catch(() => "?");
      const text = (await pageText(page)).slice(0, 160).replace(/\n+/g, " ");
      // A Next.js chunk that failed to load over a slow proxy lands the SPA
      // on its error boundary ("Something went wrong. Please try again.").
      // That page is recoverable — one reload re-boots the SPA and retries
      // the chunk; failing the round here would burn a perfectly good run.
      if (/something went wrong/i.test(text)) {
        log("SPA 落到错误页（chunk 加载失败类），整页重载再试…");
        await page.send("Page.reload").catch(() => {});
        await sleep(8000);
        continue;
      }
      const stillBusy = netBusy(15_000);
      throw new Error(`登录步骤停滞（页面：${text} · URL：${String(where).slice(0, 120)}` +
        (stillBusy ? " · 网络仍在传输：出口过慢，建议更换出口" : " · 网络已静默：流程真死") + "）");
    }
    log(`登录步骤：${state}`);
    if (state === "email") {
      // The submit's POST can die mid-flight on a flaky exit and the SPA
      // silently drops back to the email form. A healthy first click clears
      // this screen within seconds — only re-submit when it lingers, so a
      // working Continue is never doubled.
      if (await waitForStepGone(page, stepEmail, 8_000)) continue;
      log("邮箱步滞留（Continue 的请求可能被出口断连），重新提交…");
      const empty = await evaluate(page, `!document.querySelector('input[type="email"]')?.value`).catch(() => false);
      if (empty) await typeIntoInput(page, 'input[type="email"]', config.email);
      await clickAgreement(page);
      await clickButtonByText(page, "Continue");
      continue;
    }
    if (state === "password") {
      await answerPassword(page);
      await settleAfterStep(page, stepPassword, "password");
    } else {
      await answerCode(page);
      await settleAfterStep(page, stepCode, "code");
    }
  }
  throw new Error("登录步骤超过 6 轮仍未完成。");
}

/** waitForStepGone waits until the answered step's predicate turns false. */
async function waitForStepGone(page, test, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (!(await test().catch(() => true))) return true;
    await sleep(STEP_POLL_MS);
  }
  return false;
}

/**
 * settleAfterStep waits for the answered screen to go away; when it stays, the
 * signup path's submit is parked on Tencent's Turing drag captcha and needs
 * solving before the flow can move.
 */
async function settleAfterStep(page, stepTest, stepName = "") {
  if (await waitForStepGone(page, stepTest)) return;
  // A blocked reCAPTCHA script means the password submit can never land; bail
  // now instead of dragging 5 times into a 540s watchdog.
  if (stepName === "password" && recaptchaBlocked) {
    fail("上游 Google reCAPTCHA 门禁脚本加载失败（本出口无法访问 google.com），password 步无法提交，放弃本号。");
  }
  // --collect-gaps N: pure puzzle-position survey. Loads puzzles and template-
  // matches the gap without ever dragging — no verify calls, zero risk — to
  // measure whether the gap position repeats in patterns.
  if (args.collectGaps) {
    await collectGapSamples(page, args.collectGaps);
    fail(`缺口位置采集完成（${args.collectGaps} 张，见上方日志）。`);
  }
  // --manual-captcha: hands off entirely; the operator drags the slider while
  // we keep the verdict listener running and read Tencent's response codes.
  // This is the clean A/B: same browser/session, human input vs CDP input.
  if (args.manualCaptcha) {
    log("请手动拖动滑块完成验证（自动化袖手旁观，监听判定中）…");
    for (let i = 0; i < 10 && !captchaVerdictListener; i += 1) {
      const session = await findCaptchaSession(page);
      if (session) startVerdictListener(session);
      else await sleep(1500);
    }
    if (await waitForStepGone(page, stepTest, 180_000)) return;
    log("3 分钟内滑块未完成。");
    return;
  }
  // --patient: MiniMax's risk control sometimes needs many submit attempts
  // before one lands (operator-reported); give the widget a long leash.
  const cap = args.patient ? 12 : 5;
  for (let attempt = 0; attempt < cap; attempt += 1) {
    const solved = await solveTuringCaptcha(page);
    if (!solved) {
      // Two consecutive "slider ignored the drag" verdicts mean the IP is
      // rate-limited, not that the trajectory was off: dragging on is wasted
      // and deeper into the throttle. Fail fast so the caller can switch exit IP.
      // Two consecutive FULL-PUZZLE rejections used to bail at 2; the two-drag
      // rule makes each puzzle burn 2 verdicts, and single-candidate puzzles
      // burn one — an early bail here kills rounds the operator watched pass
      // visually. Three full puzzles rejected = the exit is really throttled.
      if (deadStreak >= 3 && !args.patient) {
        fail("滑块连续三题全候选被拒——判定为出口限流，放弃本号。");
      }
      // The widget may still be rendering its images; give it a beat and
      // retry before concluding it is not there.
      await sleep(3000);
      if (await waitForStepGone(page, stepTest)) return;
      continue;
    }
    log(`验证码拖拽完成（第 ${attempt + 1} 次），等待验证…`);
    if (await waitForStepGone(page, stepTest)) return;
  }
  log(`警告：验证码拖了 ${cap} 次，步骤仍未推进。`);
}

// GAP_MATCH_EXPRESSION template-matches the puzzle piece against the
// background inside the captcha iframe and returns {gapX, score, piece, bg}
// in one evaluation. Shared by the drag solver and the position survey.
const GAP_MATCH_EXPRESSION = `(async () => {
  const bgEl = document.getElementById("slideBg");
  const fgItems = [...document.querySelectorAll(".tc-fg-item")];
  const pieceEl = fgItems.find(e => {
    const r = e.getBoundingClientRect();
    return r.width > 40 && r.height > 40 && Math.abs(r.width - r.height) < 12 && !e.querySelector(".tc-slider-bg");
  });
  if (!bgEl || !pieceEl) return JSON.stringify({ err: "missing elements" });
  const load = (src) => new Promise((res, rej) => { const i = new Image(); i.crossOrigin = "anonymous"; i.onload = () => res(i); i.onerror = () => rej(new Error("img load fail")); i.src = src; });
  const bgUrls = (root) => {
    const out = [];
    const scan = (el) => {
      if (!el) return;
      const m = (getComputedStyle(el).backgroundImage || "").match(/url\\("?([^")]+)"?\\)/);
      if (m && !/^data:image\\/png;base64,iVBORw0KGgoAAAANSUhEUgAAAA/.test(m[1])) out.push(m[1]);
      for (const child of el.children || []) scan(child);
    };
    scan(root);
    return [...out, ...[...root.querySelectorAll("img")].map(i => i.src).filter(s => s && !s.startsWith("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAA"))];
  };
  const bgUrl = bgUrls(bgEl)[0] || null;
  const pieceStyle = getComputedStyle(pieceEl);
  const pieceImg = pieceEl.querySelector("img, canvas");
  const pieceBg = (pieceStyle.backgroundImage || "").match(/url\\("?([^")]+)"?\\)/);
  const pieceUrl = (pieceImg && pieceImg.src) || (pieceBg && pieceBg[1]) || "";
  if (!bgUrl || !pieceUrl) return JSON.stringify({ err: "missing images" });
  const piecePos = pieceStyle.backgroundPosition.split(" ").map(v => parseFloat(v) || 0);
  const pieceSize = pieceStyle.backgroundSize.split(" ").map(v => parseFloat(v) || 0);
  const [bgImg, pImg] = await Promise.all([load(bgUrl), load(pieceUrl)]);
  const bw = Math.round(bgEl.getBoundingClientRect().width);
  const bh = Math.round(bgEl.getBoundingClientRect().height);
  const elW = Math.round(pieceEl.getBoundingClientRect().width);
  const elH = Math.round(pieceEl.getBoundingClientRect().height);
  const spriteScale = pieceSize[0] ? pieceSize[0] / pImg.naturalWidth : (pieceSize[1] ? pieceSize[1] / pImg.naturalHeight : 1);
  const bc = document.createElement("canvas"); bc.width = bw; bc.height = bh;
  const bctx = bc.getContext("2d", { willReadFrequently: true }); bctx.drawImage(bgImg, 0, 0, bw, bh);
  const pc = document.createElement("canvas"); pc.width = elW; pc.height = elH;
  const pctx = pc.getContext("2d", { willReadFrequently: true });
  pctx.drawImage(pImg,
    -piecePos[0] / spriteScale, -piecePos[1] / spriteScale,
    elW / spriteScale, elH / spriteScale,
    0, 0, elW, elH);
  const B = bctx.getImageData(0, 0, bw, bh).data;
  const P = pctx.getImageData(0, 0, elW, elH).data;
  let bestX = -1, bestScore = Infinity;
  const y0 = Math.max(0, Math.round(pieceEl.getBoundingClientRect().y - bgEl.getBoundingClientRect().y));
  for (let x = 0; x + elW <= bw && y0 + elH <= bh; x += 2) {
    let score = 0, n = 0;
    for (let py = 0; py < elH; py += 3) {
      for (let dx = 0; dx < elW; dx += 3) {
        const bi = ((y0 + py) * bw + (x + dx)) * 4, pi = (py * elW + dx) * 4;
        if (P[pi + 3] < 40) continue;
        score += Math.abs(B[bi] - P[pi]) + Math.abs(B[bi + 1] - P[pi + 1]) + Math.abs(B[bi + 2] - P[pi + 2]);
        n++;
      }
    }
    if (n > 50 && score / n < bestScore) { bestScore = score / n; bestX = x; }
  }
  if (bestX < 0) return JSON.stringify({ err: "no match" });
  const rect = (el) => { const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height }; };
  // Raw pixel dumps for offline matcher tuning (--collect-gaps writes them to
  // captcha-debug/): base64 of the RGBA buffers, no PNG encoder needed.
  const b64 = (u8) => { let s = ""; for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000)); return btoa(s); };
  return JSON.stringify({
    gapX: bestX, bgW: bw, score: Math.round(bestScore),
    piece: rect(pieceEl), bg: rect(bgEl),
    dbg: {
      w: bw, h: bh, ew: elW, eh: elH, y0,
      bg: b64(bctx.getImageData(0, 0, bw, bh).data),
      piece: b64(pctx.getImageData(0, 0, elW, elH).data),
    },
  });
})()`;

// PIECE_STATE_EXPRESSION reads the puzzle piece's live viewport position plus
// the slider handle's. Shared by the drag closed loop and the post-release
// nudge pass — the piece rect is the ONLY trustworthy landing signal (the
// handle can sit exactly on target while the piece falls short).
const PIECE_STATE_EXPRESSION = `(() => {
  const items = [...document.querySelectorAll(".tc-fg-item")];
  const piece = items.find(e => {
    const r = e.getBoundingClientRect();
    return r.width > 40 && r.height > 40 && Math.abs(r.width - r.height) < 12 && !e.querySelector(".tc-slider-bg");
  });
  if (!piece) return null;
  const handle = items.find(e => e.className.includes("tc-slider-normal"));
  const r = piece.getBoundingClientRect();
  return JSON.stringify({
    x: r.x,
    handleX: handle && handle.getBoundingClientRect ? handle.getBoundingClientRect().x : null,
  });
})()`;

/**
 * collectGapSamples surveys puzzle gap positions without ever dragging: match
 * the piece against the background, click the widget's refresh button, repeat.
 * No verify call is made, so the session accrues no rejection history. The
 * operator wants to know whether the gap position is random per puzzle or
 * sticky across rounds.
 */
async function collectGapSamples(page, count) {
  await ensureBrowserSocket();
  const samples = [];
  for (let i = 0; i < count; i += 1) {
    const session = await findCaptchaSession(page);
    if (!session) {
      log(`[gap] 第 ${i + 1} 张：找不到拼图 iframe，等待后重试。`);
      await sleep(3000);
      i -= 1;
      if (samples.length === 0 && i > 6) fail("始终找不到拼图 iframe。");
      continue;
    }
    if (i === 0) await sleep(1500);
    const solve = await browserSock.send(
      "Runtime.evaluate",
      {
        expression: GAP_MATCH_EXPRESSION,
        awaitPromise: true,
        returnByValue: true,
      },
      session,
    ).catch((e) => ({ error: { message: e.message } }));
    if (solve.error) {
      log(`[gap] 第 ${i + 1} 张：求值失败 ${solve.error.message}`);
      await sleep(2000);
      continue;
    }
    const data = JSON.parse(solve.result.value);
    if (data.err) {
      log(`[gap] 第 ${i + 1} 张：${data.err}，刷新重试。`);
      await refreshPuzzle(session);
      continue;
    }
    const pieceStart = Math.round((data.piece.x - data.bg.x) / (data.bg.w / data.bgW));
    samples.push({ gapX: data.gapX, score: data.score, pieceStart, bgW: data.bgW });
    log(`[gap] 第 ${i + 1}/${count} 张：gapX=${data.gapX}（score ${data.score}，piece 起点 ${pieceStart}，bg 宽 ${data.bgW}）`);
    // 原图落盘，供离线调匹配器（只采集不拖拽，本命令零 verify 风险）
    if (data.dbg) {
      try {
        fs.mkdirSync("captcha-debug", { recursive: true });
        fs.writeFileSync(`captcha-debug/sample-${i + 1}.meta.json`, JSON.stringify({
          gapX: data.gapX, score: data.score, pieceStart, bgW: data.bgW,
          piece: data.piece, bg: data.bg, dims: { w: data.dbg.w, h: data.dbg.h, ew: data.dbg.ew, eh: data.dbg.eh, y0: data.dbg.y0 },
        }));
        fs.writeFileSync(`captcha-debug/sample-${i + 1}.bg.rgba`, Buffer.from(data.dbg.bg, "base64"));
        fs.writeFileSync(`captcha-debug/sample-${i + 1}.piece.rgba`, Buffer.from(data.dbg.piece, "base64"));
        // 完整 DOM + 逐元素 canvas 帧内容：验证「缺口剪影是否画在独立 canvas 上」
        const dom = await browserSock.send("Runtime.evaluate", {
          expression: `(() => {
            const out = { canvases: [], bgs: [], imgs: [] };
            for (const c of document.querySelectorAll("canvas")) {
              try {
                const r = c.getBoundingClientRect();
                out.canvases.push({ cls: String(c.className).slice(0, 40), w: c.width, h: c.height, x: Math.round(r.x), y: Math.round(r.y), png: c.toDataURL("image/png") });
              } catch (e) { out.canvases.push({ err: String(e) }); }
            }
            for (const el of document.querySelectorAll("*")) {
              const m = ((getComputedStyle(el).backgroundImage || "")).match(/url\\("?([^")]+)"?\\)/);
              if (m && !m[1].startsWith("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAA")) {
                const r = el.getBoundingClientRect();
                out.bgs.push({ tag: el.tagName, cls: String(el.className).slice(0, 40), url: m[1].slice(0, 160), x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) });
              }
            }
            for (const im of document.querySelectorAll("img")) {
              const r = im.getBoundingClientRect();
              out.imgs.push({ src: (im.src || "").slice(0, 160), x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) });
            }
            return JSON.stringify(out);
          })()`,
          returnByValue: true,
        }, session).catch(() => null);
        if (dom?.result?.value) {
          const parsed = JSON.parse(dom.result.value);
          fs.writeFileSync(`captcha-debug/sample-${i + 1}.dom.json`, JSON.stringify(parsed, null, 1));
          for (let ci = 0; ci < parsed.canvases.length; ci += 1) {
            const c = parsed.canvases[ci];
            if (c.png) {
              const [, b64] = c.png.split(",");
              fs.writeFileSync(`captcha-debug/sample-${i + 1}.canvas${ci}.${c.w}x${c.h}.png`, Buffer.from(b64, "base64"));
            }
          }
        }
        log(`[gap] 样本 ${i + 1} 已落盘 captcha-debug/`);
      } catch (error) {
        log(`[gap] 样本落盘失败：${error.message}`);
      }
    }
    if (i < count - 1) {
      await refreshPuzzle(session);
      await sleep(3500);
    }
  }
  const xs = samples.map((s) => s.gapX);
  const uniq = [...new Set(xs)];
  const dupes = xs.length - uniq.length;
  log(`[gap] ==== 采集 ${samples.length} 张，去重后 ${uniq.length} 个位置，重复 ${dupes} 次 ====`);
  log(`[gap] 位置序列: ${xs.join(", ")}`);
  log(`[gap] 位置集合: ${uniq.sort((a, b) => a - b).join(", ")}`);
}

async function refreshPuzzle(session) {
  await browserSock.send("Runtime.evaluate", {
    expression: `(() => {
      const icons = [...document.querySelectorAll(".tc-action-icon, [class*=refresh], [class*=reload]")];
      const target = icons.find(i => i.getBoundingClientRect().y < 100) || icons[0];
      if (target) { target.click(); return "ok"; }
      return "none";
    })()`,
    returnByValue: true,
  }, session).catch(() => {});
}

/**
 * findCaptchaSession locates the visible Turing captcha's OOPIF target and
 * returns an attached session for it, or null. Two same-URL gtimg iframes are
 * mounted (hidden loader + visible puzzle); attaching by URL alone is a coin
 * flip, so each candidate is probed for the actual widget DOM.
 */
async function findCaptchaSession(page) {
  const frame = await evaluate(page, `(() => {
    const f = [...document.querySelectorAll("iframe")].find(f => /turing\\.captcha\\.qcloud\\.com|captcha\\.gtimg\\.com/.test(f.src));
    if (!f) return null;
    const r = f.getBoundingClientRect();
    if (r.width < 50 || r.y < 0) return null; // still hidden off-screen
    return JSON.stringify({ x: r.x, y: r.y, w: r.width, h: r.height });
  })()`).catch(() => null);
  if (!frame) return null;

  await ensureBrowserSocket();
  const targets = await browserSock.send("Target.getTargets", {}).catch((e) => {
    log(`Target.getTargets 失败：${e.message}`);
    return { targetInfos: [] };
  });
  const candidates = (targets.targetInfos || []).filter(
    (t) => t.type === "iframe" && /turing\.captcha\.qcloud\.com|captcha\.gtimg\.com/.test(t.url),
  );
  for (const candidate of candidates) {
    const attached = await browserSock.send("Target.attachToTarget", {
      targetId: candidate.targetId,
      flatten: true,
    }).catch(() => null);
    if (!attached) continue;
    const sessionId = attached.sessionId;
    await browserSock.send("Runtime.enable", {}, sessionId);
    const probe = await browserSock.send("Runtime.evaluate", {
      expression: `JSON.stringify({ piece: Boolean(document.querySelector(".tc-fg-item")) })`,
      returnByValue: true,
    }, sessionId).catch(() => null);
    if (probe?.result?.value && JSON.parse(probe.result.value).piece) return sessionId;
  }
  return null;
}

/**
 * startVerdictListener watches the widget's own verification traffic — its
 * response body carries Tencent's verdict code (0 + ticket = pass, otherwise
 * the rejection reason), turning "the drag failed" from a guess into a fact.
 */
function startVerdictListener(captchaSession) {
  // Always on (was verbose-gated): the verify body's verdict code is the
  // ground truth every batch report is built from — cheap to keep listening.
  if (captchaVerdictListener) return;
  browserSock.send("Network.enable", {}, captchaSession).catch(() => {});
  captchaVerdictListener = (message) => {
    try {
      const parsed = JSON.parse(message.data);
      if (parsed.method === "Network.requestWillBeSent" && /captcha|gtimg|qcloud/.test(parsed.params.request.url) && parsed.params.request.method === "POST")
        log(`[cap-net] → ${parsed.params.request.url.slice(0, 90)} | ${JSON.stringify(parsed.params.request.postData || "").slice(0, 200)}`);
      if (parsed.method === "Network.requestWillBeSent" && /cap_union_new_verify/.test(parsed.params.request.url))
        verifyRequestIds.set(parsed.params.requestId, parsed.params.request.url);
      if (parsed.method === "Network.loadingFinished" && verifyRequestIds.has(parsed.params.requestId)) {
        const reqId = parsed.params.requestId;
        verifyRequestIds.delete(reqId);
        browserSock.send("Network.getResponseBody", { requestId: reqId }, captchaSession)
          .then((body) => {
            log(`[cap-verdict] ← ${String(body.body || "").slice(0, 260)}`);
            try {
              const code = (JSON.parse(body.body || "{}").errorCode ?? null);
              if (code !== null) lastVerdict = { code: String(code), at: Date.now() };
            } catch {}
          })
          .catch(() => {});
      }
      if (parsed.method === "Network.responseReceived" && /cap_union_new_verify/.test(parsed.params.response.url) && parsed.params.response.status !== 200)
        log(`[cap-net] ← ${parsed.params.response.status} verify`);
    } catch {}
  };
  browserSock.socket.addEventListener("message", captchaVerdictListener);
}

/**
 * The Turing captcha runs in an out-of-process iframe ("Slide to complete the
 * puzzle"): its submit token only exists after the slider drags the puzzle
 * piece onto the gap. The gap is the region the piece was cut from, so
 * template-matching the piece image against the background locates it, and the
 * rest is a drag performed with real mouse events.
 */
async function solveTuringCaptcha(page) {
  const frameRect = await (async () => {
    // 新版模板 (2026-10-07 观测)挂在 turing.captcha.qcloud.com/template/drag_ele.html，
    // 旧版挂在 captcha.gtimg.com —— 两个都要认，否则帧定位静默失败空转。
    const frame = await evaluate(page, `(() => {
      const f = [...document.querySelectorAll("iframe")].find(f => /turing\\.captcha\\.qcloud\\.com|captcha\\.gtimg\\.com/.test(f.src));
      if (!f) return null;
      const r = f.getBoundingClientRect();
      if (r.width < 50 || r.y < 0) return null; // still hidden off-screen
      return JSON.stringify({ x: r.x, y: r.y, w: r.width, h: r.height });
    })()`).catch(() => null);
    return frame ? JSON.parse(frame) : null;
  })();
  if (!frameRect) return false;

  await ensureBrowserSocket();
  const captchaSession = await findCaptchaSession(page);
  if (!captchaSession) {
    log("验证码弹窗已出现，但没找到拼图组件的 iframe target，等待重试。");
    return false;
  }
  await startVerdictListener(captchaSession);
  await sleep(1500); // the OOPIF session drops the first input right after attach

  // Template-match the piece against the background inside the iframe.
  const solve = await browserSock.send(
    "Runtime.evaluate",
    {
      expression: `(async () => {
        const bgEl = document.getElementById("slideBg");
        const fgItems = [...document.querySelectorAll(".tc-fg-item")];
        const pieceEl = fgItems.find(e => {
          const r = e.getBoundingClientRect();
          return r.width > 40 && r.height > 40 && Math.abs(r.width - r.height) < 12 && !e.querySelector(".tc-slider-bg");
        });
        const handleEl = fgItems.find(e => e.className.includes("tc-slider-normal"));
        if (!bgEl || !pieceEl || !handleEl) return JSON.stringify({ err: "missing elements" });
        const load = (src) => new Promise((res, rej) => { const i = new Image(); i.crossOrigin = "anonymous"; i.onload = () => res(i); i.onerror = () => rej(new Error("img load fail")); i.src = src; });
        const bgUrls = (root) => {
          const out = [];
          const scan = (el) => {
            if (!el) return;
            const m = (getComputedStyle(el).backgroundImage || "").match(/url\\("?([^")]+)"?\\)/);
            if (m && !/^data:image\\/png;base64,iVBORw0KGgoAAAANSUhEUgAAAA/.test(m[1])) out.push(m[1]);
            for (const child of el.children || []) scan(child);
          };
          scan(root);
          return [...out, ...[...root.querySelectorAll("img")].map(i => i.src).filter(s => s && !s.startsWith("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAA"))];
        };
        const bgUrl = bgUrls(bgEl)[0] || null;
        // The piece is drawn as a CSS background on the fg-item, not an <img>.
        const pieceStyle = getComputedStyle(pieceEl);
        const pieceImg = pieceEl.querySelector("img, canvas");
        const pieceBg = (pieceStyle.backgroundImage || "").match(/url\\("?([^")]+)"?\\)/);
        const pieceUrl = (pieceImg && pieceImg.src) || (pieceBg && pieceBg[1]) || "";
        if (!bgUrl || !pieceUrl) {
          return JSON.stringify({
            err: "missing images",
            bgCandidates: bgUrls(bgEl).map(u => u.slice(0, 90)),
            pieceBgUrl: pieceBg ? pieceBg[1].slice(0, 90) : null,
            pieceHtml: pieceEl.innerHTML.slice(0, 200),
          });
        }
        const piecePos = pieceStyle.backgroundPosition.split(" ").map(v => parseFloat(v) || 0);
        const pieceSize = pieceStyle.backgroundSize.split(" ").map(v => parseFloat(v) || 0);
        const [bgImg, pImg] = await Promise.all([load(bgUrl), load(pieceUrl)]);
        // Work entirely in DISPLAYED pixels: the bg is stretched into its
        // element box, and the piece is a sprite window (background-size scales
        // the sheet, background-position shifts it) cropped to the element box.
        const bw = Math.round(bgEl.getBoundingClientRect().width);
        const bh = Math.round(bgEl.getBoundingClientRect().height);
        const elW = Math.round(pieceEl.getBoundingClientRect().width);
        const elH = Math.round(pieceEl.getBoundingClientRect().height);
        const spriteScale = pieceSize[0] ? pieceSize[0] / pImg.naturalWidth : (pieceSize[1] ? pieceSize[1] / pImg.naturalHeight : 1);
        const bc = document.createElement("canvas"); bc.width = bw; bc.height = bh;
        const bctx = bc.getContext("2d", { willReadFrequently: true }); bctx.drawImage(bgImg, 0, 0, bw, bh);
        const pc = document.createElement("canvas"); pc.width = elW; pc.height = elH;
        const pctx = pc.getContext("2d", { willReadFrequently: true });
        pctx.drawImage(pImg,
          -piecePos[0] / spriteScale, -piecePos[1] / spriteScale,
          elW / spriteScale, elH / spriteScale,
          0, 0, elW, elH);
        const B = bctx.getImageData(0, 0, bw, bh).data;
        const P = pctx.getImageData(0, 0, elW, elH).data;
        // The slider only moves horizontally, so the gap's row band is the
        // piece's current row band; only x is searched.
        const y0 = Math.max(0, Math.round(pieceEl.getBoundingClientRect().y - bgEl.getBoundingClientRect().y));
        // MiniMax's widget shows more than one piece-shaped notch (operator
        // report 2026-10-07: "二选一，还有一层判断") — a plain global argmin can
        // lock onto the decoy. Keep every non-overlapping local candidate and
        // grade each with a correlation coefficient: the true hole is THIS
        // piece's own image darkened, so piece↔bg pixels correlate there; a
        // decoy notch holds different content and does not.
        const scored = [];
        for (let x = 0; x + elW <= bw && y0 + elH <= bh; x += 2) {
          let score = 0, n = 0;
          for (let py = 0; py < elH; py += 3) {
            for (let dx = 0; dx < elW; dx += 3) {
              const bi = ((y0 + py) * bw + (x + dx)) * 4, pi = (py * elW + dx) * 4;
              if (P[pi + 3] < 40) continue; // transparent part of the piece
              score += Math.abs(B[bi] - P[pi]) + Math.abs(B[bi + 1] - P[pi + 1]) + Math.abs(B[bi + 2] - P[pi + 2]);
              n++;
            }
          }
          if (n > 50) scored.push({ x, s: score / n });
        }
        scored.sort((a, b) => a.s - b.s);
        const picked = [];
        for (const c of scored) {
          if (picked.every((p) => Math.abs(p.x - c.x) > elW / 2)) picked.push(c);
          if (picked.length >= 4) break;
        }
        const corrAt = (x) => {
          let sp = 0, sb = 0, sp2 = 0, sb2 = 0, spb = 0, m = 0;
          for (let py = 0; py < elH; py += 3) {
            for (let dx = 0; dx < elW; dx += 3) {
              const pi = (py * elW + dx) * 4, bi = ((y0 + py) * bw + (x + dx)) * 4;
              if (P[pi + 3] < 40) continue;
              const pv = (P[pi] + P[pi + 1] + P[pi + 2]) / 3, bv = (B[bi] + B[bi + 1] + B[bi + 2]) / 3;
              sp += pv; sb += bv; sp2 += pv * pv; sb2 += bv * bv; spb += pv * bv; m++;
            }
          }
          if (m < 50) return 0;
          const cov = spb / m - (sp / m) * (sb / m);
          const sd = Math.sqrt(Math.max(0, sp2 / m - (sp / m) ** 2) * Math.max(0, sb2 / m - (sb / m) ** 2)) || 1;
          return Math.round((cov / sd) * 1000) / 1000;
        };
        for (const c of picked) c.corr = corrAt(c.x);
        if (!picked.length) {
          let opaque = 0;
          for (let i = 3; i < P.length; i += 4) if (P[i] >= 40) opaque++;
          return JSON.stringify({
            err: "no match",
            pieceNat: [pImg.naturalWidth, pImg.naturalHeight],
            sprite: [piecePos[0], piecePos[1], pieceSize[0], pieceSize[1], spriteScale],
            opaque, elW, elH, bw, bh,
            pieceUrlHead: pieceUrl.slice(0, 90),
          });
        }
        // ---- 候选缺口检测（2D）+ 左侧优先（captcha-debug/ 5 组真值的结论）----
        // 1) 背景图自带 ~26px 顶部边条 → 缺口格子在 y0+8..+42（实测偏移 +26/+32）
        // 2) 每张题有 2 个深色剪影缺口（真+诱饵），已知真值 5/5 都在左侧 → 先选最左
        // 3) 合格缺口：多阈值下「拼图掩码落在暗区」的命中率 ≥ 0.55，且暗区不淹没全框
        const lumAt = (bi) => 0.299 * B[bi] + 0.587 * B[bi + 1] + 0.114 * B[bi + 2];
        const mask = [];
        for (let py = 0; py < elH; py += 2) {
          for (let dx = 0; dx < elW; dx += 2) {
            if (P[(py * elW + dx) * 4 + 3] >= 40) mask.push([dx, py]);
          }
        }
        const mCount = mask.length || 1;
        const L = new Float32Array(bw * bh);
        for (let i = 0; i < bw * bh; i += 1) L[i] = lumAt(i * 4);
        const allCells = [];
        for (let py = 0; py < elH; py += 2) for (let dx = 0; dx < elW; dx += 2) allCells.push([dx, py]);
        const yLo = Math.max(0, y0 + 8), yHi = Math.min(bh - elH, y0 + 42);
        const perX = [];
        for (let x = 0; x + elW <= bw; x += 2) {
          let best = null;
          for (let y = yLo; y <= yHi; y += 2) {
            let wm = 0;
            for (const [dx, py] of allCells) wm += L[(y + py) * bw + (x + dx)];
            wm /= allCells.length;
            for (const t of [0.5, 0.62, 0.74]) {
              const th = t * wm;
              let darkBox = 0, hit = 0;
              for (const [dx, py] of allCells) if (L[(y + py) * bw + (x + dx)] < th) darkBox += 1;
              if (darkBox > allCells.length * 0.8) continue; // 整框皆暗 = 暗色画面，非缺口
              for (const [dx, py] of mask) if (L[(y + py) * bw + (x + dx)] < th) hit += 1;
              hit /= mCount;
              if (hit >= 0.5 && darkBox <= mCount * 1.8 && (!best || hit > best.hit)) {
                best = { x, y, hit: Math.round(hit * 100) / 100 };
              }
            }
          }
          if (best) perX.push(best);
        }
        // 聚类成缺口（x 间隔 >16px 分开），取每簇最高命中
        const blobs = [];
        for (const c of perX.sort((a, b) => a.x - b.x)) {
          const last = blobs[blobs.length - 1];
          if (last && c.x - last.x2 <= 16) {
            if (c.hit > last.hit) { last.x = c.x; last.y = c.y; last.hit = c.hit; }
            last.x2 = Math.max(last.x2, c.x);
          } else {
            blobs.push({ x: c.x, y: c.y, hit: c.hit, x2: c.x });
          }
        }
        // 质心精修：命中率在缺口附近 ±15px 都饱和（平台期），max-hit 点会偏离
        // 缺口正中十几像素（operator 盯屏实测）。改用「暗区质心 − 掩码质心」
        // 对齐，把命令位置钉在缺口正中。
        for (const b of blobs) {
          let best = null;
          for (let cx = Math.max(0, b.x - 20); cx + elW <= bw && cx <= b.x + 20; cx += 2) {
            for (let cy = Math.max(0, b.y - 8); cy + elH <= bh && cy <= b.y + 8; cy += 2) {
              let wm = 0;
              for (const [dx, py] of allCells) wm += L[(cy + py) * bw + (cx + dx)];
              wm /= allCells.length;
              for (const t of [0.5, 0.62, 0.74]) {
                const th = t * wm;
                let darkBox = 0, hit = 0;
                for (const [dx, py] of allCells) if (L[(cy + py) * bw + (cx + dx)] < th) darkBox += 1;
                for (const [dx, py] of mask) if (L[(cy + py) * bw + (cx + dx)] < th) hit += 1;
                hit /= mCount;
                if (darkBox <= mCount * 1.9 && hit >= (best ? best.hit : 0)) best = { x: cx, y: cy, th, hit };
              }
            }
          }
          if (!best) continue;
          let dsx = 0, dsy = 0, dn = 0, msx = 0, msy = 0;
          for (const [dx, py] of allCells) if (L[(best.y + py) * bw + (best.x + dx)] < best.th) { dsx += best.x + dx; dsy += best.y + py; dn += 1; }
          for (const [dx, py] of mask) { msx += dx; msy += py; }
          if (dn < 30) continue;
          b.rawX = b.x;
          b.x = Math.max(0, Math.min(bw - elW, Math.round(dsx / dn - msx / mCount)));
          b.y = Math.max(0, Math.min(bh - elH, Math.round(dsy / dn - msy / mCount)));
          b.hit = Math.round(best.hit * 100) / 100;
        }
        blobs.sort((a, b) => a.x - b.x);
        const blobLabel = (b) => b.x + '(hit' + b.hit.toFixed(2) + ')';
        let bestX, selector;
        if (blobs.length) {
          bestX = blobs[0].x;
          selector = '最左缺口 ' + blobLabel(blobs[0]) + ' · 共' + blobs.length + '个';
        } else {
          bestX = picked.length ? picked[0].x : 0;
          selector = '强度回退 ' + bestX;
        }
        const bestScore = picked.length ? picked[0].s : 0;
        const rect = (el) => { const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height }; };
        const bgStyle = getComputedStyle(bgEl);
        const bgPos = bgStyle.backgroundPosition.split(" ").map(v => parseFloat(v) || 0);
        const bgSizeVal = bgStyle.backgroundSize.split(" ").map(v => parseFloat(v) || 0);
        const tail = (u) => (u.match(/img_index=(\d)/) || [])[1] || u.slice(-12);
        // 原图随包返回，供线上判定打标签（verdict 是唯一真值）
        const b64 = (u8) => { let s = ""; for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000)); return btoa(s); };
        return JSON.stringify({
          gapX: bestX, bgW: bw, bgNat: [bgImg.naturalWidth, bgImg.naturalHeight],
          bgPos, bgSizeVal, bgUrlTail: tail(bgUrl), pieceUrlTail: tail(pieceUrl),
          piece: rect(pieceEl), handle: rect(handleEl),
          pieceUrlLen: pieceUrl.length, score: Math.round(bestScore),
          selector,
          blobs: blobs.map((b) => ({ x: b.x, y: b.y, hit: b.hit })),
          candidates: picked.map((c) => ({ x: c.x, score: Math.round(c.s), corr: c.corr })),
          dbg: { w: bw, h: bh, ew: elW, eh: elH, y0, bg: b64(B), piece: b64(P) },
          fgItems: fgItems.map((e) => {
            const r = e.getBoundingClientRect();
            return { cls: String(e.className).slice(0, 46), x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) };
          }),
        });
      })()`,
      awaitPromise: true,
      returnByValue: true,
    },
    captchaSession,
  ).catch((e) => ({ error: { message: e.message } }));
  if (solve.error) {
    log(`验证码求解失败：${solve.error.message}`);
    return false;
  }
  const data = JSON.parse(solve.result.value);
  if (data.err) {
    log(`验证码求解失败：${data.err} ${JSON.stringify(data).slice(0, 500)}`);
    return false;
  }
  if (data.fgItems?.length) log(`[cap-dom] fg-item×${data.fgItems.length}: ${JSON.stringify(data.fgItems)}`);
  if (data.blobs?.length) {
    const list = data.blobs.map((b) => `${b.x}/y${b.y}(hit${b.hit})`).join(" · ");
    log(`[cap-match] 缺口候选：${list} → 取 ${data.gapX}（${data.selector}）`);
  }
  // 每次求解的原图+候选落盘：verdict 回来后即为带标签样本（captcha-debug/live/）
  if (data.dbg) {
    capDumpSeq += 1;
    try {
      fs.mkdirSync("captcha-debug/live", { recursive: true });
      const base = `captcha-debug/live/attempt-${String(capDumpSeq).padStart(3, "0")}`;
      fs.writeFileSync(`${base}.bg.rgba`, Buffer.from(data.dbg.bg, "base64"));
      fs.writeFileSync(`${base}.piece.rgba`, Buffer.from(data.dbg.piece, "base64"));
      const { dbg, ...rest } = data;
      fs.writeFileSync(`${base}.meta.json`, JSON.stringify(rest));
      log(`[cap-dump] ${base}`);
    } catch (error) {
      log(`[cap-dump] 落盘失败：${error.message}`);
    }
  }

  // ---- 同题轮询候选（operator 2026-10-07 观察：失败后题目不变，刷新才换题）----
  // verdict 是唯一真值：先滑最左候选，被拒不刷新、直接滑下一个候选；全被拒才换题。
  const bgRect = await browserSock.send("Runtime.evaluate", {
    expression: `(() => { const r = document.getElementById("slideBg").getBoundingClientRect(); return JSON.stringify({ x: r.x, y: r.y, w: r.width, h: r.height }); })()`,
    returnByValue: true,
  }, captchaSession);
  const bg = JSON.parse(bgRect.result.value);
  const scale = bg.w / data.bgW; // =1（bgW 即显示宽度），保留以防模板缩放
  // ---- 一张图两次，先左后右（operator 规则 2026-10-07）----
  // ① 排除拼图起点自带暗影 ghost：背景图在拼图起始位置烤着一块拼图形状的暗
  //    影，blob 检测必中它（s5 的 x=47、attempt-002 的 x=4 都是它），直接丢弃。
  // ② 每题恰好两个真缺口：取 hit 最高的两个候选，按 x 升序 = 先左后右。
  // ③ 两次都被拒才刷新换题（失败后题目不变，刷新才换）。
  const pieceStart0 = (data.piece.x - bg.x) / scale;
  const realBlobs = (data.blobs || []).filter((b) => Math.abs(b.x - pieceStart0) > 40);
  const targets = (realBlobs.length
    ? [...realBlobs].sort((a, b) => (b.hit || 0) - (a.hit || 0)).slice(0, 2).sort((a, b) => a.x - b.x)
    : [{ x: data.gapX, hit: 0 }]);
  log(`[cap-plan] 同题两拖（先左后右）：${targets.map((b) => b.x + '(' + (b.hit ?? 0).toFixed?.(2) + ')').join(" → ")}${(data.blobs || []).length !== realBlobs.length ? ` · 已排除起点ghost（piece起点 ${pieceStart0.toFixed(0)}）` : ""}`);
  for (let ti = 0; ti < targets.length; ti += 1) {
    const gapX = targets[ti].x;
    const gapXDisplay = bg.x + gapX * scale;
    // 被拒不重置的模板里拼图可能留在上一次的位置；每次都现读
    const st = await readPieceX(browserSock, captchaSession);
    if (!st) return true; // 控件已收起 = 通过
    const pieceStart = (st.x - bg.x) / scale;
    const dragPx = (gapX - pieceStart) * scale;
    if (dragPx < 5 || dragPx > bg.w - data.handle.w) {
      log(`[cap-plan] 候选 ${gapX} 位移不合理（${Math.round(dragPx)}px），跳过。`);
      continue;
    }
    const handleCenterX = (st.handleX != null ? st.handleX : bg.x) + data.handle.w / 2;
    const startY = data.handle.y + data.handle.h / 2;
    log(`缺口候选 ${gapX}${targets[ti].rawX != null ? `（原始 ${targets[ti].rawX}，质心精修）` : ""}，需移动 ${Math.round(dragPx)}px（第 ${ti + 1}/${targets.length} 个）。`);
    const dragStartAt = Date.now();
    // Input events must be dispatched on the iframe's OWN session: out-of-process
    // iframes do not receive the page's synthesized mouse events.
    await dragSliderClosedLoop(browserSock, captchaSession, handleCenterX, startY, dragPx, gapXDisplay);
    // 等 verdict：控件收起 = 通过；errorCode "0" = 通过；其余 = 同题换下一个候选
    const code = await waitForVerdict(captchaSession, dragStartAt, 6000);
    if (code === "0") {
      log(`缺口 ${gapX} 判定通过。`);
      deadStreak = 0; // this widget responded; the IP is not throttled right now
      await clickVerifyOk(captchaSession);
      return true;
    }
    log(`缺口 ${gapX} 判定 ${code ?? "无响应"}，${ti + 1 < targets.length ? "同题换下一个候选。" : "候选已用尽。"}`);
  }
  // 同题候选全部被拒 → 刷新换题重来（deadStreak 按整题计）
  deadStreak += 1;
  log("同题候选全部被拒，刷新验证码…");
  await refreshPuzzle(captchaSession);
  await sleep(2000);
  return false;
}

/** waitForVerdict polls for the drag's outcome. The widget's OWN success/
 * failure text is the primary signal (the network verdict body is sometimes
 * unfetchable — misreading a visible pass as a rejection drags the piece off
 * the correct notch); the verify response code is the fallback. The widget
 * collapsing mid-wait also counts as acceptance. */
async function waitForVerdict(captchaSession, since, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (lastVerdict.at >= since) return lastVerdict.code;
    const visual = await browserSock.send("Runtime.evaluate", {
      expression: `(() => {
        const t = document.body.innerText || "";
        if (/verification passed|验证通过/i.test(t)) return "passed";
        if (/verification failed|验证失败|try again/i.test(t)) return "failed";
        return "none";
      })()`,
      returnByValue: true,
    }, captchaSession).catch(() => null);
    if (visual?.result?.value === "passed") return "0";
    if (visual?.result?.value === "failed") return "visual-50";
    const piece = await readPieceX(browserSock, captchaSession).catch(() => null);
    if (!piece) return "0";
    await sleep(250);
  }
  return null;
}

/** clickVerifyOk presses the widget's OK button when the variant shows one. */
async function clickVerifyOk(captchaSession) {
  const coords = await browserSock.send("Runtime.evaluate", {
    expression: `(() => {
      const btn = document.querySelector(".verify-btn") ||
        [...document.querySelectorAll("button, div")].find(e => /^(ok|verify)$/i.test((e.innerText || "").trim()));
      if (!btn) return null;
      const r = btn.getBoundingClientRect();
      if (r.width < 5 || r.height < 5) return null;
      return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
    })()`,
    returnByValue: true,
  }, captchaSession).catch(() => null);
  if (!coords?.result?.value) return;
  const { x, y } = JSON.parse(coords.result.value);
  log("点击验证码的 OK 按钮…");
  await dragMouse(browserSock, captchaSession, x, y, 0);
}

/**
 * dragMouse performs a human-shaped drag: accelerating then easing out, with
 * vertical jitter — the trajectory is part of what the captcha scores. It runs
 * on whichever session owns the target (`sock`, `session`).
 */
async function dragMouse(sock, session, startX, startY, distance) {
  await dragTrajectory(sock, session, startX, startY, distance);
  await releaseDrag(sock, session, startX + distance, startY);
}

/** releaseDrag lifts the button at (x, y) on the owning session. */
async function releaseDrag(sock, session, x, y) {
  try {
    await sock.send("Input.dispatchMouseEvent", {
      type: "mouseReleased", x: Math.round(x), y: Math.round(y),
      button: "left", buttons: 0, clickCount: 1, pointerType: "mouse",
    }, session);
  } catch (error) {
    log(`[warn] 鼠标事件派发失败：${error.message}`);
  }
}

/** dragTrajectory presses and walks the handle to startX+distance, button still down. */
async function dragTrajectory(sock, session, startX, startY, distance) {
  const cdpSend = async (params) => {
    try {
      await sock.send("Input.dispatchMouseEvent", params, session);
    } catch (error) {
      log(`[warn] 鼠标事件派发失败：${error.message}`);
    }
  };
  // Real drags carry the pressed-button state on every move and an explicit
  // pointer type; CDP defaults omit both, which the captcha's telemetry reads
  // as synthetic input.
  const move = (x, y, buttons) => cdpSend({ type: "mouseMoved", x, y, buttons, pointerType: "mouse" });
  await move(startX, startY, 0);
  await cdpSend({ type: "mousePressed", x: startX, y: startY, button: "left", buttons: 1, clickCount: 1, pointerType: "mouse" });
  await sleep(150 + Math.random() * 180); // the grab: press, then a beat
  // NOTE: The widget only tracks the piece when every move carries an integer-
  // pixel step: sub-pixel eased curves leave it glued to the start while press
  // and release still register. This shape measured 80% in the afternoon —
  // resist "improving" it without a fresh A/B.
  let moved = 0;
  const steps = 34 + Math.floor(Math.random() * 14);
  for (let i = 1; i <= steps; i += 1) {
    const t = i / steps;
    // slow start, quick middle, gentle landing
    const eased = t < 0.2 ? t * 1.6 : t < 0.8 ? 0.32 + (t - 0.2) * 1.35 : 1.128 - Math.pow(1 - t, 2.4) * 0.9;
    const target = Math.max(0, Math.min(distance, Math.round(distance * eased)));
    const jitterY = (Math.random() - 0.5) * 4;
    await move(startX + target, startY + jitterY, 1);
    moved = target;
    await sleep(11 + Math.floor(Math.random() * 30));
  }
  // Micro-corrections near the gap, the way a person settles the piece.
  for (const back of [-3, 2, -1]) {
    if (Math.abs(distance) < 6) break;
    await move(startX + moved + back, startY + (Math.random() - 0.5) * 2, 1);
    await sleep(40 + Math.random() * 60);
  }
  await move(startX + distance, startY, 1);
}

// ---------------- 落点闭环（Qoder2api 滑块攻坚总结 2026-10-06 的落地） ----------------
// 三条实测规律：CDP 合成输入零误差（落点偏差不是派发的锅）；拼图的视觉落点
// 系统性偏短且收敛是事件驱动——静置多久都没用，连发 pointermove 才会逼近目标；
// 整步修正实测 ~1.6× 过冲振荡，半步（×0.5）阻尼才能稳收敛。本站（腾讯
// Turing）额外一条本地实测：控件只认整数像素步进（见 dragTrajectory 的
// NOTE），所以微移动用 ±1px 整数抖动代替总结里的 ±0.3px。
const CAP_AIM_BIAS = 0.5;    // 通过样本普遍落在缺口右侧 +0.4~+0.5px，瞄准点略过一点
const CAP_RESIDUAL_STOP = 3; // |残差| ≤ 3px 即收手

/** readPieceX parses the live piece/handle position, null when the widget is gone. */
async function readPieceX(sock, session) {
  const landed = await sock.send("Runtime.evaluate", {
    expression: PIECE_STATE_EXPRESSION,
    returnByValue: true,
  }, session).catch(() => null);
  if (!landed?.result?.value) return null;
  try {
    return JSON.parse(landed.result.value);
  } catch {
    return null;
  }
}

/** microBurst fires integer-jitter micro-moves: convergence is event-driven,
 * so pumping events (net displacement ≈ 0) is what walks the lag out. */
async function microBurst(sock, session, cx, cy, count) {
  for (let i = 0; i < count; i += 1) {
    const dx = i % 2 ? 0 : (Math.random() < 0.5 ? -1 : 1);
    const dy = Math.random() < 0.25 ? (Math.random() < 0.5 ? -1 : 1) : 0;
    try {
      await sock.send("Input.dispatchMouseEvent", {
        type: "mouseMoved", x: cx + dx, y: cy + dy, buttons: 1, pointerType: "mouse",
      }, session);
    } catch {}
    await sleep(35 + Math.random() * 75);
  }
}

/** dragSteps walks a correction over 4-6 small steps instead of one jump. */
async function dragSteps(sock, session, fromX, y, delta) {
  const steps = 4 + Math.floor(Math.random() * 3);
  for (let i = 1; i <= steps; i += 1) {
    try {
      await sock.send("Input.dispatchMouseEvent", {
        type: "mouseMoved", x: fromX + Math.round((delta * i) / steps), y,
        buttons: 1, pointerType: "mouse",
      }, session);
    } catch {}
    await sleep(25 + Math.random() * 45);
  }
}

/**
 * dragSliderClosedLoop is one full drag: main trajectory to the target, then
 * converge (micro-move burst) → read the piece's real landing → damped half-
 * step correction, up to 5 rounds, stopping at |residual| ≤ 3px; a final burst,
 * a short rest, then release. Every round is logged so batch runs double as
 * landing forensics (期望 vs 实测). Returns { trail, gone }.
 */
async function dragSliderClosedLoop(sock, session, startX, startY, distance, gapXDisplay) {
  await dragTrajectory(sock, session, startX, startY, distance);
  let handleX = startX + distance;
  const trail = [];
  for (let round = 0; round < 5; round += 1) {
    await microBurst(sock, session, handleX, startY, round === 0 ? 30 : 20);
    await sleep(120); // 收敛事件发完、静定之后的回读才可靠（拖拽中途读会吃到动画滞后）
    const piece = await readPieceX(sock, session);
    if (!piece) return { trail, gone: true }; // 控件中途消失 = 已判定通过
    const residual = gapXDisplay + CAP_AIM_BIAS - piece.x;
    trail.push(Math.round(residual * 10) / 10);
    log(`[cap-loop] 轮${round + 1} · 手柄 ${(handleX - startX).toFixed(0)}px · 拼图实测 ${piece.x.toFixed(1)} · 残差 ${residual.toFixed(1)}px`);
    if (Math.abs(residual) <= CAP_RESIDUAL_STOP) break;
    const prev = trail[trail.length - 2];
    if (round > 0 && Math.abs(residual) > Math.abs(prev) - 1 && Math.abs(residual) > 15) {
      log(`[cap-loop] 拼图对移动无响应（残差 ${residual.toFixed(1)}px 不再收敛），提前松手。`);
      break;
    }
    const corr = Math.round(residual * 0.5); // 半步阻尼：实测增益 ≤2× 都收敛
    if (corr === 0) break; // 亚像素残差只能靠微事件磨，整步会振荡
    await dragSteps(sock, session, handleX, startY, corr);
    handleX += corr;
  }
  await microBurst(sock, session, handleX, startY, 8);
  await sleep(200 + Math.random() * 150); // 释放前长时间驻留没用，短静置即可
  await releaseDrag(sock, session, handleX, startY);
  log(`[cap-drag] 期望 ${gapXDisplay.toFixed(1)}px · 释放手柄 ${(handleX - startX).toFixed(0)}px · 残差迹线 ${trail.join(" → ") || "（首轮即达标）"}`);
  return { trail, gone: false };
}

/**
 * answerPassword answers the password step. The page encrypts the password
 * itself; the typed value is all it needs.
 */
async function answerPassword(page) {
  log("需要密码，正在输入…");
  // Registration variants render a confirm field; fill every password box the
  // step shows so "password + confirm password" is covered the same way.
  const count = await evaluate(page, `document.querySelectorAll('input[type="password"]').length`);
  for (let i = 0; i < count; i += 1) {
    const coords = await evaluate(page, `(() => {
      const el = document.querySelectorAll('input[type="password"]')[${i}];
      if (!el) return null;
      el.scrollIntoView({ block: "center" });
      const r = el.getBoundingClientRect();
      return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
    })()`);
    if (!coords) break;
    await clearAndType(page, JSON.parse(coords), config.password);
  }
  await clickAgreement(page);
  await clickButtonByText(page, "Continue");
}

/**
 * answerCode answers a verification-code step: poll the mailbox for a MiniMax
 * code newer than this run's start, then type it. This is the branch fresh
 * registrations land in.
 */
async function answerCode(page) {
  if (!config.mailKey) fail("登录页要求邮箱验证码，但未提供 --mail-key（无法读取验证码邮件）。");
  log("需要邮箱验证码，正在从邮箱 API 获取…");
  if (verbose) {
    const buttons = await evaluate(page, `JSON.stringify([...document.querySelectorAll("button")].map(b => ({
      t: (b.innerText || "").trim().slice(0, 24) || "[空]", d: b.disabled,
    })))`).catch(() => "[]");
    log(`[verbose] 验证码页按钮：${buttons}`);
    const text = (await pageText(page)).slice(0, 240).replace(/\n+/g, " | ");
    log(`[verbose] 验证码页文案：${text}`);
  }
  // Some variants require pressing "Send code" first; press it when it exists
  // and is idle, otherwise the step auto-sends on entry. Real click: the page
  // ignores synthetic DOM events.
  const sendCoords = await evaluate(page, `(() => {
    const btn = [...document.querySelectorAll("button")].find(b =>
      /send|resend|get.*code|发送|获取|重新/i.test(b.innerText) && !/\\d/.test(b.innerText) && !b.disabled);
    if (!btn) return null;
    btn.scrollIntoView({ block: "center" });
    const r = btn.getBoundingClientRect();
    return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2, t: btn.innerText.trim().slice(0, 20) });
  })()`).catch(() => null);
  if (sendCoords) {
    const info = JSON.parse(sendCoords);
    log(`点击「${info.t}」发送验证码…`);
    await clickAt(page, info);
  } else {
    log("未找到发码按钮，假定该步骤自动发码。");
  }
  const startedAt = Date.now() - 60_000; // tolerate slight clock skew
  let code = await pollVerificationCode(page, startedAt);
  if (triedCodes.has(code)) {
    log(`${code} 已经试过，点 Resend 等一个新码。`);
    await clickResend(page);
    code = await pollVerificationCode(page, Date.now() - 30_000);
  }
  triedCodes.add(code);
  log(`验证码已取得：${code}`);
  ledgerUpdate(config.email, { stage: "code-received", code });
  const typed = await evaluate(page, `(() => {
    const inputs = [...document.querySelectorAll("input")].filter(i =>
      /code|验证码/i.test(i.placeholder || "") || (i.type !== "email" && i.type !== "password"));
    if (!inputs.length) return false;
    return true;
  })()`);
  if (!typed) fail("找不到验证码输入框。");
  await typeIntoInput(page, 'input:not([type="email"]):not([type="password"])', code);
  await clickAgreement(page);
  await clickButtonByText(page, "Continue");
  // 验证码已消费：从落盘里删掉（operator 要求，防下一号数据串行）
  ledgerUpdate(config.email, { stage: "code-submitted", code: undefined });
}

// ------------------------------------------------------------------ the wait

/**
 * waitForCredential polls until the SPA has finished its post-redirect boot:
 * the redirect lands on the agent origin with an ANONYMOUS token in place, and
 * only after /v1/api/user/info round-trips does the logged-in JWT (the one
 * carrying user.id) replace it. Harvesting too early would deliver a useless
 * anonymous credential, so both signals are required.
 */
async function waitForCredentialReady(page) {
  const deadline = Date.now() + NAV_TIMEOUT_MS * 2;
  let last = "";
  while (Date.now() < deadline) {
    const credential = await readCredential(page);
    if (await isSignedInToken(credential)) return credential;
    const where = await evaluate(page, "location.href").catch(() => "?");
    if (where !== last) {
      last = where;
      log(`等待回跳… 当前在 ${String(where).slice(0, 90)}`);
    }
    await sleep(STEP_POLL_MS);
  }
  throw new Error("等待登录 token 超时：回跳后始终没拿到带 user.id 的 JWT。");
}

function isSignedInToken(credential) {
  if (!credential?.token) return false;
  try {
    const [, payload] = credential.token.split(".");
    const claims = JSON.parse(atob(payload.replace(/-/g, "+").replace(/_/g, "/")));
    if (!claims?.user?.id) return false;
  } catch {
    return false;
  }
  return Boolean(credential.detail?.realUserID);
}

/** readCredential gathers the token and the fingerprint the console expects. */
async function readCredential(page) {
  const raw = await evaluate(page, `JSON.stringify({
    token: localStorage.getItem(${JSON.stringify(TOKEN_KEY)}),
    uuid: localStorage.getItem(${JSON.stringify(UUID_KEY)}),
    detail: JSON.parse(localStorage.getItem(${JSON.stringify(DETAIL_KEY)}) || "null"),
  })`).catch(() => null);
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw);
    return {
      token: (parsed.token || "").trim(),
      uuid: (parsed.uuid || "").trim(),
      detail: parsed.detail || null,
    };
  } catch {
    return null;
  }
}

async function readToken(page) {
  const credential = await readCredential(page);
  return credential?.token || null;
}

// ------------------------------------------------------------------- delivery

// ---- 账号全程落盘（operator 2026-10-07 要求：结构化、整行更新、不能乱加乱减）----
// 每个账号一行（按 email 为键），阶段推进原地更新整行：
//   registering（建号+密码）→ code-received（验证码）→ code 用完即删（防串号）
//   → completed（凭证）→ delivered（入库）| interrupted（可恢复）
// 旧版"只在成功后追加一行"导致中断轮什么都没留下、下一号数据可能串行。
const LEDGER_PATH = path.join(HERE, "..", "..", "credentials-local.jsonl");
function ledgerUpdate(email, patch) {
  if (!email) return;
  let lines = [];
  try {
    lines = fs.readFileSync(LEDGER_PATH, "utf8").split("\n").filter(Boolean);
  } catch {}
  let found = false;
  const updated = lines.map((line) => {
    let rec;
    try {
      rec = JSON.parse(line);
    } catch {
      return line;
    }
    if (rec.email !== email) return line;
    found = true;
    return JSON.stringify({ ...rec, ...patch, updatedAt: new Date().toISOString() });
  });
  if (!found) updated.push(JSON.stringify({ email, ...patch, updatedAt: new Date().toISOString() }));
  try {
    fs.writeFileSync(LEDGER_PATH, updated.join("\n") + "\n");
  } catch (error) {
    log(`[warn] 账号落盘失败：${error.message}`);
  }
}

/** persistCredential merges the harvested credential into the account's
 * existing ledger line (one line per account, in place). */
function persistCredential(credential) {
  ledgerUpdate(config.email, {
    stage: "completed",
    token: credential.token,
    uuid: credential.uuid || null,
    userId: credential.detail?.realUserID != null ? String(credential.detail.realUserID) : null,
    userName: credential.detail?.userName || null,
  });
  log("凭证已落盘 credentials-local.jsonl");
}

async function deliver(credential) {
  const payload = {
    token: credential.token,
    uuid: credential.uuid,
    region: "global",
    identifier: credential.detail?.userName || config.email,
    group: args.group || "login-pipeline",
    remark: `tools/login.mjs 自动登录（${config.email}）`,
  };
  if (credential.detail?.realUserID) payload.userId = String(credential.detail.realUserID);
  // request() uses Node's global fetch, which ignores the browser proxy and
  // dials the console directly; a transient direct-network blip surfaces as
  // "fetch failed" and used to discard an already-registered account (token in
  // hand, never delivered). Retry a few times before giving up.
  let lastError;
  for (let attempt = 1; attempt <= 3; attempt += 1) {
    try {
      const response = await request("/admin/api/accounts", { method: "POST", body: payload });
      const account = response?.account || {};
      log(`已入库：${account.id} · ${account.name} · 状态 ${account.status}`);
      ledgerUpdate(config.email, { stage: "delivered", accountId: account.id, deliveredAt: new Date().toISOString() });
      return;
    } catch (err) {
      lastError = err;
      log(`投递失败（第 ${attempt}/3 次）：${err.message}`);
      if (attempt < 3) await sleep(3000 * attempt);
    }
  }
  throw lastError;
}

// ----------------------------------------------------------------- mail API

const MAIL_SUBDOMAINS = [
  "sub.7447.site",
  "aa.7447.site",
  "mio.7447.site",
  "xxx.7447.site",
  "x.7447.site",
  "inbox.7447.site",
  "tmp.7447.site",
  "box.7447.site",
  "mail2.7447.site",
  "m.7447.site",
  "test.7447.site",
];
const MAIL_APEX = "7447.site";

// unsnow is the backup provider (mail.unsnow.org, Bearer auth): its 5 domains
// rotate alongside 7447's, used only after every 7447 domain has failed.
const UNSNOW_BASE = "https://mail.unsnow.org";
const UNSNOW_DOMAINS = [
  "sms.snowsmtp.com",
  "mx.unsnow.online",
  "mail.unsnow.org",
  "snow.edu.kg",
  "ldc-free.com",
];

/**
 * domainUsage counts how many accounts each candidate domain already holds in
 * the local ledger, so the least-used domain is tried first (ported from the
 * StepFun pickDomains rotation). Every row counts — delivered or interrupted —
 * so a domain that has been hammered stays deprioritised after failures too.
 */
function domainUsage(domains) {
  const counts = new Map(domains.map((d) => [d, 0]));
  try {
    const text = fs.readFileSync(LEDGER_PATH, "utf8");
    for (const line of text.split("\n")) {
      const match = line.match(/@([A-Za-z0-9.-]+)/);
      if (match && counts.has(match[1])) counts.set(match[1], counts.get(match[1]) + 1);
    }
  } catch {}
  return [...counts.entries()].sort((a, b) => a[1] - b[1]);
}

/** createMailbox7447 registers a mailbox on mail.7447.site (X-API-Key auth). */
async function createMailbox7447(domain) {
  const response = await jsonRequest(`${MAIL_BASE}/v1/accounts`, {
    method: "POST",
    headers: { "X-API-Key": config.mailKey, "Content-Type": "application/json" },
    body: JSON.stringify(domain ? { domain } : {}),
  });
  const address = response?.data?.address || response?.address;
  if (!address) throw new Error(`7447 建址无地址：${JSON.stringify(response).slice(0, 160)}`);
  return { address, provider: "7447" };
}

/** createMailboxUnsnow registers a mailbox on mail.unsnow.org (Bearer auth). */
async function createMailboxUnsnow(domain) {
  if (!config.unsnowKey) throw new Error("缺少 unsnow key");
  const response = await jsonRequest(`${UNSNOW_BASE}/api/v1/mailboxes`, {
    method: "POST",
    headers: { Authorization: `Bearer ${config.unsnowKey}`, "Content-Type": "application/json" },
    body: JSON.stringify(domain ? { domain } : {}),
  });
  if (!response?.address) throw new Error(`unsnow 建址无地址：${JSON.stringify(response).slice(0, 160)}`);
  return { address: response.address, provider: "unsnow", mailboxId: response.id };
}

/**
 * createMailbox rotates domains least-used-first and fails over to the next on
 * error — 7447 subdomains + apex first, then unsnow as the backup provider.
 * Returns { address, provider, mailboxId? } so the code poller knows which API
 * to read. An explicit --mail-domain keeps the old single-domain (+apex) path.
 */
async function createMailbox() {
  if (!config.mailKey) fail("--new-mailbox 需要 --mail-key。");

  const fixed = config.mailDomain && config.mailDomain !== "random" && config.mailDomain !== "all"
    ? config.mailDomain : null;
  if (fixed) {
    try {
      return await createMailbox7447(fixed);
    } catch (err) {
      log(`[mail] 指定域名 ${fixed} 创建失败（${err.message}），回退到默认主域…`);
      return await createMailbox7447(null);
    }
  }

  const provider = config.mailProvider || "auto";
  const attempts = [];
  if (provider === "7447" || provider === "auto")
    for (const [domain, used] of domainUsage([...MAIL_SUBDOMAINS, MAIL_APEX]))
      attempts.push({ make: createMailbox7447, domain, used, tag: "7447" });
  if (provider === "unsnow" || provider === "auto")
    for (const [domain, used] of domainUsage(UNSNOW_DOMAINS))
      attempts.push({ make: createMailboxUnsnow, domain, used, tag: "unsnow" });

  let lastError = null;
  for (const attempt of attempts) {
    try {
      const box = await attempt.make(attempt.domain);
      log(`[mail] 新建邮箱：${box.address}（${attempt.tag} · 域 ${attempt.domain}，此前已用 ${attempt.used} 个）`);
      return box;
    } catch (err) {
      lastError = err;
      log(`[mail] 域 ${attempt.domain}（${attempt.tag}）建址失败：${err.message}，换下一个…`);
    }
  }
  fail(`所有域建址均失败${lastError ? `（最后错误：${lastError.message}）` : ""}`);
}

/**
 * pollVerificationCode reads the mailbox until a MiniMax code email newer than
 * `sinceMs` shows up. The send sometimes lands minutes late, and first sends
 * sometimes never arrive at all, so the wait is long and periodically presses
 * the page's Resend button.
 */
async function pollVerificationCode(page, sinceMs) {
  const deadline = Date.now() + CODE_TIMEOUT_MS;
  let resent = 0;
  let lastError = "";
  while (Date.now() < deadline) {
    try {
      if (config.mailProvider === "unsnow") {
        const code = await fetchUnsnowCode();
        if (code) return code;
      } else {
        const url = `${MAIL_BASE}/v1/messages?address=${encodeURIComponent(config.email)}&limit=20`;
        const response = await jsonRequest(url, { headers: { "X-API-Key": config.mailKey } });
        const messages = response?.data?.messages || [];
        for (const message of messages) {
          // The mail API returns timezone-less UTC ("2026-10-04 20:05:50");
          // Date.parse alone would read it as local time, skewing by the
          // timezone offset and filtering every fresh code out as stale.
          const createdRaw = String(message.created_at || message.date || "").replace(" ", "T");
          const created = Date.parse(/(?:Z|[+-]\d{2}:?\d{2})$/.test(createdRaw) ? createdRaw : `${createdRaw}Z`) || 0;
          if (created && created < sinceMs) continue; // older than this run
          if (!/mail\.minimax/i.test(message.from || "")) continue;
          const detail = await jsonRequest(
            `${MAIL_BASE}/v1/messages/${message.id}?address=${encodeURIComponent(config.email)}`,
            { headers: { "X-API-Key": config.mailKey } },
          );
          const raw = detail?.data?.raw || "";
          const code = extractVerificationCode(raw);
          if (code) return code;
        }
      }
      log("还没有验证码邮件，继续等待…");
    } catch (error) {
      lastError = error.message;
    }
    // The countdown button turns back into "Resend" once it expires; press it
    // every ~90s — first sends sometimes never land.
    if (Date.now() - (deadline - CODE_TIMEOUT_MS) > 90_000 * (resent + 1) && resent < 3) {
      resent += 1;
      log(`等了约 ${resent * 1.5} 分钟还没到，点一次 Resend。`);
      await clickResend(page);
    }
    await sleep(CODE_POLL_MS);
  }
  fail(`等待验证码超时${lastError ? `（最后错误：${lastError}）` : ""}`);
}

/**
 * fetchUnsnowCode reads the unsnow mailbox and returns the first 6-digit code.
 * The body lives in `intro` ("YOUR VERIFICATION CODE IS: 123456 …"), not in
 * raw/text, so the candidate order mirrors the provider doc.
 */
async function fetchUnsnowCode() {
  const url = `${UNSNOW_BASE}/api/v1/mailboxes/${config.mailboxId}/messages`;
  const response = await jsonRequest(url, { headers: { Authorization: `Bearer ${config.unsnowKey}` } });
  const list = response?.["hydra:member"] || response?.messages || [];
  for (const message of list) {
    const body = message.intro || message.text || message.html || message.subject || message.raw || "";
    const code = extractVerificationCode(String(body));
    if (code) return code;
  }
  return null;
}

/** clickResend presses the resend button once the countdown has expired. */
async function clickResend(page) {
  const coords = await evaluate(page, `(() => {
    const btn = [...document.querySelectorAll("button")].find(b =>
      /resend|重新发送|重新/i.test(b.innerText) && !b.disabled);
    if (!btn) return null;
    const r = btn.getBoundingClientRect();
    return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
  })()`).catch(() => null);
  if (!coords) {
    log("Resend 按钮不可用（可能还在倒计时）。");
    return;
  }
  await clickAt(page, JSON.parse(coords));
}

/**
 * extractVerificationCode pulls the 6-digit code out of a MIME email. The raw
 * string mixes headers with encoded HTML — headers contain their own six-digit
 * fragments, so the body is isolated, HTML stripped and quoted-printable
 * decoded before any digits are read. The code sits next to the word "code"
 * when the template holds.
 */
function extractVerificationCode(raw) {
  const body = raw.split(/\r?\n\r?\n/).slice(1).join("\n\n") || raw;
  const candidates = [];
  const consider = (text) => {
    const plain = String(text)
      .replace(/<style[\s\S]*?<\/style>/gi, " ")
      .replace(/<[^>]+>/g, " ")
      .replace(/=\r?\n/g, "");
    try {
      candidates.push(quopriDecode(plain));
    } catch {
      candidates.push(plain);
    }
  };
  consider(body);
  // base64 body parts decode as a fallback for templates that hide the text.
  for (const blob of body.match(/[A-Za-z0-9+/=]{200,}/g) || []) {
    try {
      consider(Buffer.from(blob, "base64").toString("utf8"));
    } catch {}
  }
  for (const text of candidates) {
    const nearCode = text.match(/code[^0-9]{0,40}(\d{6})|(\d{6})[^0-9]{0,40}code/i);
    if (nearCode) return nearCode[1] || nearCode[2];
  }
  for (const text of candidates) {
    const first = text.match(/\b(\d{6})\b/);
    if (first) return first[1];
  }
  return null;
}

/** quopriDecode decodes quoted-printable text without pulling in a MIME lib. */
function quopriDecode(text) {
  if (!/=[0-9A-F]{2}/.test(text)) return text;
  const bytes = [];
  for (let i = 0; i < text.length; i += 1) {
    const char = text[i];
    if (char === "=" && /[0-9A-F]{2}/.test(text.slice(i + 1, i + 3))) {
      bytes.push(parseInt(text.slice(i + 1, i + 3), 16));
      i += 2;
    } else {
      bytes.push(char.charCodeAt(0) & 0xff);
    }
  }
  return Buffer.from(bytes).toString("utf8");
}

async function jsonRequest(url, options = {}) {
  // Node's global fetch has NO default timeout. The mail API's message-detail
  // endpoint measured 14s via curl (and far longer via Node), so a stalled call
  // used to block the code poll forever — the `while (Date.now() < deadline)`
  // check never re-ran and the round died on the 540s watchdog with the code
  // sitting unread in the mailbox. Bound every request.
  const { timeoutMs = 20_000, ...init } = options;
  const response = await fetch(url, { ...init, signal: AbortSignal.timeout(timeoutMs) });
  const text = await response.text();
  if (!response.ok) throw new Error(`HTTP ${response.status}: ${text.slice(0, 160)}`);
  try {
    return text ? JSON.parse(text) : null;
  } catch {
    throw new Error("返回的不是 JSON");
  }
}

// ----------------------------------------------------------------- interaction

/**
 * The page's widgets are React-controlled, so synthetic DOM events do not move
 * their state. CDP's own input pipeline (insertText + real mouse coordinates)
 * is indistinguishable from a human as far as the page is concerned, and is
 * what makes this flow survive their frontend updates.
 */
async function sendKeys(page, text) {
  await cdpSend(page, "Input.insertText", { text });
}

async function clickButtonByText(page, text) {
  const coords = await evaluate(page, `(() => {
    const find = (exact) => [...document.querySelectorAll("button")].find(b =>
      (exact ? b.innerText.trim() === ${JSON.stringify(text)} : b.innerText.trim().includes(${JSON.stringify(text)})) && !b.disabled);
    const btn = find(true) || find(false);
    if (!btn) return null;
    btn.scrollIntoView({ block: "center" });
    const r = btn.getBoundingClientRect();
    return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
  })()`);
  if (!coords) fail(`找不到「${text}」按钮。`);
  await clickAt(page, JSON.parse(coords));
}

async function clickAgreement(page) {
  // The agreement state carries across steps, and the box is a toggle with no
  // aria attributes: checked renders an <svg> inside, unchecked a bordered
  // <div>. Clicking an already-checked box would UNcheck it and silently
  // block Continue, so only press it while unchecked.
  const coords = await evaluate(page, `(() => {
    const box = [...document.querySelectorAll("button")].find(b => b.className.includes("size-4"));
    if (!box) return null;
    if (box.querySelector("svg")) return "already";
    const r = box.getBoundingClientRect();
    return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
  })()`);
  if (!coords || coords === "already") return;
  await clickAt(page, JSON.parse(coords));
}

async function clickAt(page, { x, y }, clickCount = 1) {
  await cdpSend(page, "Input.dispatchMouseEvent", { type: "mouseMoved", x, y });
  await cdpSend(page, "Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", clickCount });
  await cdpSend(page, "Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", clickCount });
}

/**
 * clearAndType focuses the field with a triple click (which selects whatever a
 * previous attempt left behind) and types over it.
 */
async function clearAndType(page, coords, text) {
  await clickAt(page, coords, 3);
  await sendKeys(page, text);
}

/**
 * waitForAny polls the alternatives in order and resolves with the first name
 * whose test passes; null when the deadline passes.
 */
async function waitForAny(page, alternatives, timeout = STEP_TIMEOUT_MS) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    for (const alternative of alternatives) {
      if (await alternative.test().catch(() => false)) return alternative.name;
    }
    await sleep(STEP_POLL_MS);
  }
  return null;
}

async function hasSelector(page, selector) {
  return evaluate(page, `Boolean(document.querySelector(${JSON.stringify(selector)}))`).catch(() => false);
}

async function pageText(page) {
  return evaluate(page, "document.body.innerText").catch(() => "");
}

// --------------------------------------------------------------------- chrome
// Same shape as capture.mjs: one dedicated profile, one owned instance, every
// exit path closes it.

let browser = null;
let chromeChild = null;
// The page websocket speaks for one target only; reaching into the captcha's
// out-of-process iframe needs a browser-level connection of its own.
let browserSock = null;
let captchaVerdictListener = null;
// requestId → url of in-flight cap_union_new_verify requests, for fetching
// their response bodies (Tencent's verdict codes).
const verifyRequestIds = new Map();
// requestId → url of in-flight account.minimax.io/oauth2/* requests — their
// response bodies tell success from silent risk-control rejection (the SPA
// white-screens without an error when the login is refused).
const oauth2RequestIds = new Map();
// Consecutive drags the widget ignored outright (throttle symptom, see
// settleAfterStep's early bail). Cleared whenever a drag lands.
let deadStreak = 0;
// Sequence number for live attempt dumps (captcha-debug/live/attempt-NNN.*)
let capDumpSeq = 0;
// Latest Turing verify verdict: { code: "0"|"50"|"9"|..., at: Date.now() }.
// The widget keeps the SAME puzzle after a rejection — the verdict is what
// tells us to try the next candidate notch instead of refreshing.
let lastVerdict = { code: null, at: 0 };
// Codes already typed this process — a stale repeat must not be re-entered.
const triedCodes = new Set();
// Live network-activity tracker for the stall check: on a slow proxy the
// post-code OAuth round trip keeps loading long after the step timeouts, and
// declaring "stalled" then kills a round that was about to succeed.
let netInFlight = 0;
let netLastEventAt = Date.now();
// Set when MiniMax's Google reCAPTCHA Enterprise script fails to load. The
// password step's submit waits on it, so the page just hangs ("验证码拖了 5 次")
// until the watchdog. Residential exits that block google.com hit this every
// round; failing fast turns a 540s stall into a couple of seconds and lets the
// caller switch exit instead of burning the round.
let recaptchaBlocked = false;
// Ad/analytics beacons retry ENDLESSLY when blocked (meerkat-reporter,
// google-analytics, twitter ads…); counting them makes the wire look
// permanently busy and the stall check would wait the full window watching
// tracker chatter while the actual login POST died long ago.
const TELEMETRY_RE = /google-analytics|googletagmanager|doubleclick|adsct|analytics\.twitter|bat\.bing|alb\.reddit|t\.co\/|meerkat-reporter|rum-openway|guance\.com|sentry|\/collect\?/i;
/** netBusy: requests still in flight, or wire active within the window. */
function netBusy(quietWindowMs = 15_000) {
  return netInFlight > 0 || Date.now() - netLastEventAt < quietWindowMs;
}

async function ensureBrowser() {
  if (await debuggerUp()) {
    log("复用已打开的浏览器实例。");
    browser = { port: config.port, reused: true };
    await ensureBrowserSocket();
    return;
  }
  const executable = findChrome();
  if (!executable) fail("找不到 Chrome，请用 CHROME_PATH 指定浏览器可执行文件");
  fs.mkdirSync(config.profile, { recursive: true });
  chromeChild = spawn(
    executable,
    [
      `--remote-debugging-port=${config.port}`,
      `--user-data-dir=${config.profile}`,
      "--no-first-run",
      "--no-default-browser-check",
      // Registration profiles are throwaway; Edge on a Microsoft-signed-in OS
      // silently links the OS account and starts syncing (welcome popup steals
      // focus mid-automation, and the real identity pollutes the session).
      "--disable-sync",
      // CalculateNativeWinOcclusion is what made "the window must stay in
      // front": Windows marks an off-screen or covered window as hidden →
      // visibilityState=hidden → rAF stalls → the slider never moves. With it
      // off the window can sit off-screen and still run at full speed.
      "--disable-features=msImplicitSignin,msSigninProfileLinking,EdgeSync,SyncSynchronizer,msAccountProfileImageUpload,CalculateNativeWinOcclusion",
      // Belt-and-braces anti-throttle: timers/rAF keep full rate even when the
      // window is occluded or unfocused.
      "--disable-backgrounding-occluded-windows",
      "--disable-renderer-backgrounding",
      "--disable-background-timer-throttling",
      // The signup submit waits on an invisible captcha that never returns a
      // token for a navigator.webdriver browser; this flag keeps it quiet in
      // both headless and headed modes.
      "--disable-blink-features=AutomationControlled",
      ...(args.headed
        ? [
            // Parked past the left edge of the 1440x900 desktop: invisible to
            // the operator, yet (with the flags above) treated as active.
            `--window-position=${args.windowPosition || "-2400,160"}`,
            `--window-size=${args.windowSize || "1100,820"}`,
          ]
        : [
            "--headless=new",
            // Headless announces itself as "HeadlessChrome" in the UA, which
            // the Turing captcha treats as an automatic reject regardless of
            // the drag. Headed real browsers keep their genuine UA — masking
            // it would only degrade the fingerprint we are borrowing.
            `--user-agent=${stealthUA()}`,
          ]),
      ...(config.proxy ? [`--proxy-server=${config.proxy}`] : []),
      "about:blank",
    ],
    { detached: true, stdio: "ignore" },
  );
  chromeChild.unref();
  browser = { port: config.port, reused: false };
  const started = Date.now();
  while (Date.now() - started < NAV_TIMEOUT_MS) {
    if (await debuggerUp()) break;
    await sleep(300);
  }
  if (!(await debuggerUp())) {
    await closeBrowser().catch(() => {});
    throw new Error("浏览器启动了但调试端口没有监听");
  }
  await ensureBrowserSocket();
}

async function ensureBrowserSocket() {
  if (browserSock) return;
  const version = await fetchJson(`http://127.0.0.1:${config.port}/json/version`);
  browserSock = await attach(version.webSocketDebuggerUrl);
  // Learn the real engine version so the UA override stays plausible.
  const match = /Chrome\/(\d+)/.exec(version.Browser || "");
  if (match) browserMajorVersion = Number(match[1]);
}

async function openPage(url) {
  const res = await fetch(`http://127.0.0.1:${config.port}/json/new?${encodeURIComponent(url)}`, {
    method: "PUT",
  });
  if (!res.ok) throw new Error(`无法打开页面：HTTP ${res.status}`);
  const target = await res.json();
  const page = await attach(target.webSocketDebuggerUrl);
  await page.send("Runtime.enable");
  await page.send("Page.enable");
  // 网络轨迹常开（oauth2 响应体 + net-fail 是空白页排查的生命线）；吵闹的
  // 广告 POST 日志仍归 verbose 管。
  await enableNetworkTrace(page);
  // Headless defaults to 800x600, which clips the agreement checkbox below the
  // fold; coordinates outside the viewport swallow mouse events silently.
  await page.send("Emulation.setDeviceMetricsOverride", {
    width: 1280,
    height: 900,
    deviceScaleFactor: 1,
    mobile: false,
  });
  return page;
}

/** enableNetworkTrace logs login-related requests and responses to stdout. */
async function enableNetworkTrace(page) {
  await page.send("Network.enable");
  // requestId → URL：net-fail 行要指认是哪条导航被中断
  const pendingNetUrls = new Map();
  page.socket.addEventListener("message", (event) => {
    let message;
    try {
      message = JSON.parse(event.data);
    } catch {
      return;
    }
    if (message.method === "Network.requestWillBeSent") {
      const request = message.params.request;
      pendingNetUrls.set(message.params.requestId, request.url);
      if (!TELEMETRY_RE.test(request.url)) {
        netInFlight += 1;
        netLastEventAt = Date.now();
      }
      if (verbose && request.method === "POST")
        log(`[net] → ${request.url.slice(0, 90)} | ${JSON.stringify(request.postData || "").slice(0, 200)}`);
      // oauth2/login 的响应体是登录成功/被风控的唯一判据（SPA 白屏时不抛错）
      if (/account\.minimax\.io\/oauth2\//.test(request.url)) {
        oauth2RequestIds.set(message.params.requestId, request.url);
      }
    }
    if (message.method === "Network.loadingFinished") {
      const doneUrl = pendingNetUrls.get(message.params.requestId) || "";
      if (!TELEMETRY_RE.test(doneUrl)) {
        netInFlight = Math.max(0, netInFlight - 1);
        netLastEventAt = Date.now();
      }
      if (oauth2RequestIds.has(message.params.requestId)) {
        const reqId = message.params.requestId;
        const url = oauth2RequestIds.get(reqId);
        oauth2RequestIds.delete(reqId);
        page.send("Network.getResponseBody", { requestId: reqId })
          .then((body) => log(`[net-body] ← ${String(url).slice(0, 70)} | ${String(body.body || "").slice(0, 220)}`))
          .catch(() => {});
      }
    }
    if (message.method === "Network.responseReceived") {
      const response = message.params.response;
      if (/minimax\.io\/oauth2/.test(response.url))
        log(`[net] ← ${response.status} ${response.url.slice(0, 80)}`);
    }
    if (message.method === "Network.loadingFailed") {
      // net-fail 带上 URL：空白页排查需要知道是哪条导航被中断
      const url = pendingNetUrls.get(message.params.requestId) || "?";
      if (/google\.com\/recaptcha\/enterprise\.js/.test(url)) {
        recaptchaBlocked = true;
        log(`[recaptcha] 门禁脚本加载失败（${message.params.errorText}）——本出口访问不了 google.com，password 步将无法提交。`);
      }
      if (!TELEMETRY_RE.test(url)) {
        netInFlight = Math.max(0, netInFlight - 1);
        netLastEventAt = Date.now();
      }
      log(`[net-fail] ${message.params.errorText} ${message.params.blockedReason || ""} ${url.slice(0, 110)}`);
    }
    if (message.method === "Runtime.exceptionThrown") {
      const details = message.params.exceptionDetails;
      const where = `${details.url || "?"}:${details.lineNumber || "?"}`;
      const text = details.exception?.description || details.text || "unknown";
      log(`[js-err] ${where} · ${text.slice(0, 220)}`);
    }
    if (message.method === "Runtime.consoleAPICalled" && message.params.type === "error") {
      const parts = (message.params.args || []).map((a) => a.value ?? a.description ?? "").join(" ");
      log(`[console] ${parts.slice(0, 220)}`);
    }
  });
}

async function closeBrowser() {
  if (browser) {
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
  if (chromeChild?.pid) {
    try {
      if (process.platform === "win32") {
        execFileSync("taskkill.exe", ["/pid", String(chromeChild.pid), "/T", "/F"], { stdio: "ignore" });
      } else {
        process.kill(chromeChild.pid, "SIGKILL");
      }
    } catch {}
    chromeChild = null;
  }
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
    // sessionId (when given) is a top-level CDP field: browser-level sockets
    // use it to route commands into attached targets such as OOPIF iframes.
    send(method, params = {}, sessionId) {
      const id = ++nextId;
      return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        const message = { id, method, params };
        if (sessionId) message.sessionId = sessionId;
        ws.send(JSON.stringify(message));
      });
    },
    close() {
      ws.close();
    },
    socket: ws,
  };
}

/**
 * evaluate resolves against the page's current target, re-attaching if a
 * navigation invalidated the previous session — the OAuth round trip redirects
 * across origins several times.
 */
async function evaluate(page, expression) {
  const { result, exceptionDetails } = await page.send("Runtime.evaluate", {
    expression,
    returnByValue: true,
    awaitPromise: true,
  });
  if (exceptionDetails) throw new Error(exceptionDetails.text || "页面求值失败");
  return result.value;
}

// cdpSend is evaluate's sibling for methods that must go through the same
// page session (Input.* and friends) without needing a return value.
function cdpSend(page, method, params) {
  return page.send(method, params).then(() => true, () => false);
}

// ------------------------------------------------------------------ http calls

async function request(pathname, options = {}) {
  const headers = { authorization: `Bearer ${config.key}` };
  if (options.body !== undefined) headers["content-type"] = "application/json";
  const response = await fetch(`${config.base}${pathname}`, {
    method: options.method || "GET",
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    // First direct connection to the console measured 83s; bound it so a stall
    // fails fast and the caller's retry (which reuses a warm socket) wins.
    signal: AbortSignal.timeout(options.timeoutMs || 60_000),
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
  const parsed = {};
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const value = () => argv[++i];
    const flag = (name) => {
      parsed[name] = true;
    };
    if (arg === "--email") parsed.email = value();
    else if (arg === "--password") parsed.password = value();
    else if (arg === "--mail-key") parsed.mailKey = value();
    else if (arg === "--unsnow-key") parsed.unsnowKey = value();
    else if (arg === "--mail-domain") parsed.mailDomain = value();
    else if (arg === "--mail-provider") parsed.mailProvider = value();
    else if (arg === "--base") parsed.base = value();
    else if (arg === "--key") parsed.key = value();
    else if (arg === "--config") parsed.config = value();
    else if (arg === "--profile") parsed.profile = value();
    else if (arg === "--port") parsed.port = Number(value());
    else if (arg === "--proxy") parsed.proxy = value();
    else if (arg === "--group") parsed.group = value();
    else if (arg === "--window-position") parsed.windowPosition = value();
    else if (arg === "--window-size") parsed.windowSize = value();
    else if (arg === "--new-mailbox") flag("newMailbox");
    else if (arg === "--headed") flag("headed");
    else if (arg === "--patient") flag("patient");
    else if (arg === "--collect-gaps") parsed.collectGaps = Number(value());
    else if (arg === "--manual-captcha") flag("manualCaptcha");
    else if (arg === "--keep-open") flag("keepOpen");
    else if (arg === "--no-deliver") flag("noDeliver");
    else if (arg === "--once") flag("once");
    else if (arg === "--verbose") flag("verbose");
    else if (arg === "--help" || arg === "-h") {
      console.log(
        [
          "用法：node tools/login.mjs [选项]",
          "",
          "  --email ADDR      登录邮箱（必填）",
          "  --password PW     登录密码（密码登录必填）",
          "  --mail-key KEY    邮箱 API 密钥（验证码登录 / --new-mailbox 需要）",
          "  --mail-domain DOM 指定邮箱域名/子域（默认在 12 个子域+主域中随机轮换）",
          "  --new-mailbox     先用邮箱 API 创建一个全新邮箱再登录（注册新号）",
          "  --base URL        控制台地址（默认读配置文件 / MINIMAX2API_BASE）",
          "  --key KEY         管理员密钥（默认读配置文件 / MINIMAX2API_ADMIN_KEY）",
          "  --config FILE     配置文件路径（默认 capture.local.json）",
          "  --profile DIR     Chrome 配置目录（默认与抓取助手共用）",
          "  --port N          调试端口（默认 9227）",
          "  --proxy URL       传给浏览器的代理（默认读 HTTPS_PROXY）",
          "  --group NAME      入库后的账号分组",
          "  --no-deliver      只登录取凭证，不入库",
          "  --once            完成后退出（默认）",
          "  --verbose         输出调试信息",
        ].join("\n"),
      );
      process.exit(0);
    } else {
      fail(`无法识别的参数：${arg}`);
    }
  }
  return parsed;
}

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
  const port = Number(parsed.port || stored.port || 9227);
  const proxy = parsed.proxy || process.env.LOGIN_PROXY || process.env.HTTPS_PROXY || process.env.HTTP_PROXY || "";
  return {
    email: parsed.email || stored.email || "",
    password: parsed.password || stored.password || "",
    mailKey: parsed.mailKey || stored.mailKey || "",
    unsnowKey: parsed.unsnowKey || process.env.MINIMAX2API_UNSNOW_KEY || stored.unsnowKey || "",
    mailDomain: parsed.mailDomain || stored.mailDomain || null,
    mailProvider: parsed.mailProvider || stored.mailProvider || "auto",
    base: (parsed.base || process.env.MINIMAX2API_BASE || stored.base || "").replace(/\/$/, ""),
    key: parsed.key || process.env.MINIMAX2API_ADMIN_KEY || stored.key || "",
    profile: parsed.profile || stored.profile || defaultProfile(parsed),
    port: Number.isFinite(port) ? port : 9227,
    proxy,
    group: parsed.group || "",
    source: file || "",
  };
}

/**
 * Registrations get their own throwaway profile, separate from the capture
 * profile whose session the operator's harvests depend on.
 */
function defaultProfile(parsed) {
  const name = parsed?.newMailbox ? "minimax2api-register" : "minimax2api-capture-profile";
  return path.join(defaultRoot(), name);
}

/**
 * killDebuggerOwner stops a headless instance left behind by an earlier run
 * that still holds the profile lock — without this, the fresh spawn would
 * silently defer to it and inherit its stale session.
 */
async function killDebuggerOwner() {
  if (process.platform !== "win32") return;
  try {
    const stdout = execFileSync("cmd.exe", ["/c", `netstat -ano | findstr :${config.port}`], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
    for (const line of stdout.split("\n")) {
      const parts = line.trim().split(/\s+/);
      const pid = parts[parts.length - 1];
      if (pid && /^\d+$/.test(pid) && pid !== "0" && pid !== String(process.pid)) {
        try { execFileSync("taskkill.exe", ["/pid", pid, "/T", "/F"], { stdio: "ignore" }); } catch {}
      }
    }
  } catch {}
}

function findConfigFile(parsed) {
  const candidates = [
    parsed.config,
    process.env.MINIMAX2API_CAPTURE_CONFIG,
    path.join(HERE, "..", "..", "capture.local.json"),
    path.join(process.cwd(), "capture.local.json"),
  ].filter(Boolean);
  return candidates.find((candidate) => fs.existsSync(candidate)) || "";
}

function defaultRoot() {
  return process.platform === "win32"
    ? process.env.LOCALAPPDATA || path.join(os.homedir(), "AppData", "Local")
    : path.join(os.homedir(), ".cache");
}

function log(message) {
  console.log(`[${new Date().toTimeString().slice(0, 8)}] ${message}`);
}

function fail(message) {
  const error = new Error(message);
  error.expected = true;
  throw error;
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    void closeBrowser().finally(() => process.exit(0));
  });
}

main()
  .then(() => (args.keepOpen ? process.exit(0) : closeBrowser().finally(() => process.exit(0))))
  .catch(async (error) => {
    // 中断也落盘：阶段、密码、（未消费的）验证码都留在档上，可恢复可审计
    ledgerUpdate(config.email, { stage: "interrupted", lastError: String(error?.message || error).slice(0, 200) });
    console.error(error?.expected ? error.message : `登录管线异常退出：${error?.message || error}`);
    if (!args.keepOpen) await closeBrowser().catch(() => {});
    process.exit(1);
  });
