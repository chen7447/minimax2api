// Batch runner driver: runs multiple batches of sweep.mjs in sequence.
//
//   node tools/batch-runner.mjs [--start-batch 2] [--total-batches 10]
//                               [--batch-size 10] [--interval 120]
//                               [--mail-key KEY] [--headed] [--verbose]
import { spawn } from "node:child_process";
import fs from "node:fs";

const args = {};
for (let i = 2; i < process.argv.length; i += 1) {
  const a = process.argv[i];
  const v = () => process.argv[++i];
  if (a === "--start-batch") args.startBatch = Number(v());
  else if (a === "--total-batches") args.totalBatches = Number(v());
  else if (a === "--batch-size") args.batchSize = Number(v());
  else if (a === "--interval") args.interval = Number(v());
  else if (a === "--mail-key") args.mailKey = v();
  else if (a === "--window-position") args.windowPosition = v();
  else if (a === "--window-size") args.windowSize = v();
  else if (a === "--headed") args.headed = true;
  else if (a === "--verbose") args.verbose = true;
  else if (a === "--require-google") args.requireGoogle = true;
  else if (a === "--pin") args.pin = true;
  else { console.log(`无法识别参数：${a}`); process.exit(2); }
}

args.startBatch ??= 1;
args.totalBatches ??= 10;
args.batchSize ??= 10;
args.interval ??= 180; // seconds between accounts in each batch (放宽冷却防卡顿)
args.windowPosition ??= "-2400,160"; // parked off the left edge (no desktop occlusion)
args.windowSize ??= "1100,820";
args.mailKey ??= "mb_517202663a2458e8330e160e50b7a09f3544ff8e79572bc7e785e2fc61cb8437";

const stamp = () => new Date().toTimeString().slice(0, 8);
const log = (m) => {
  const line = `[${stamp()}] ${m}`;
  console.log(line);
  try { fs.appendFileSync("batch-summary.log", line + "\n"); } catch {}
};

function runBatch(batchNum) {
  return new Promise((resolve) => {
    log(`\n======================================================`);
    log(`🚀 开始执行第 ${batchNum}/${args.totalBatches} 批次（本批目标 ${args.batchSize} 个账号，--headed）`);
    log(`======================================================`);

    const sweepArgs = [
      "tools/sweep.mjs",
      "--mail-key", args.mailKey,
      "--rounds", String(args.batchSize),
      "--interval", String(args.interval),
      ...(args.windowPosition ? ["--window-position", args.windowPosition] : []),
      ...(args.windowSize ? ["--window-size", args.windowSize] : []),
      ...(args.headed ? ["--headed"] : []),
      ...(args.verbose ? ["--verbose"] : []),
      ...(args.requireGoogle ? ["--require-google"] : []),
      ...(args.pin ? ["--pin"] : []),
    ];

    const child = spawn(process.execPath, sweepArgs, {
      stdio: ["ignore", "pipe", "pipe"],
      cwd: process.cwd(),
    });

    let out = "";
    child.stdout.on("data", (d) => {
      const s = d.toString();
      out += s;
      process.stdout.write(s);
    });
    child.stderr.on("data", (d) => {
      const s = d.toString();
      out += s;
      process.stderr.write(s);
    });

    child.on("exit", (code) => {
      const summaryMatch = out.match(/==== 汇总：(\d+)\/(\d+) 成功 ====/);
      const ok = summaryMatch ? Number(summaryMatch[1]) : 0;
      const total = summaryMatch ? Number(summaryMatch[2]) : args.batchSize;
      log(`🏁 第 ${batchNum} 批次完成：${ok}/${total} 成功（Exit Code: ${code}）`);
      resolve({ batchNum, ok, total, code, out });
    });
  });
}

async function main() {
  log(`=== 批量压测调度启动：从第 ${args.startBatch} 批到第 ${args.totalBatches} 批，每批 ${args.batchSize} 个账号 ===`);
  const batchRecords = [];

  for (let b = args.startBatch; b <= args.totalBatches; b += 1) {
    const res = await runBatch(b);
    batchRecords.push(res);

    const totalOk = batchRecords.reduce((acc, r) => acc + (r.ok || 0), 0);
    const totalCount = batchRecords.reduce((acc, r) => acc + (r.total || 0), 0);
    const rate = ((totalOk / totalCount) * 100).toFixed(1);
    log(`📊 截至第 ${b} 批累计战绩：${totalOk}/${totalCount} 成功（成功率 ${rate}%）`);

    if (b < args.totalBatches) {
      log(`[batch] 第 ${b} 批完成，冷却 60 秒后启动第 ${b + 1} 批…`);
      await new Promise((r) => setTimeout(r, 60_000));
    }
  }

  log(`\n🎉🎉🎉 全部 ${args.totalBatches} 批次终极检验执行完毕！`);
  const finalOk = batchRecords.reduce((acc, r) => acc + (r.ok || 0), 0);
  const finalCount = batchRecords.reduce((acc, r) => acc + (r.total || 0), 0);
  log(`🏆 终榜汇总：${finalOk}/${finalCount} 成功（${((finalOk / finalCount) * 100).toFixed(1)}%）`);
}

main().catch((e) => {
  log(`[ERROR] 批次调度器异常: ${e.message}`);
  process.exit(2);
});
