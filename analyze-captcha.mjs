// 2D template search: piece sprite vs bg, luminance-NCC and gradient-NCC.
// Ground truth: sample4 true x∈[168,218](sprite box), sample5 true x∈[125,175].
import fs from "node:fs";

const DIR = "captcha-debug";
const lum = (b, i) => 0.299 * b[i] + 0.587 * b[i + 1] + 0.114 * b[i + 2];

function search(n) {
  const meta = JSON.parse(fs.readFileSync(`${DIR}/sample-${n}.meta.json`, "utf8"));
  const bg = fs.readFileSync(`${DIR}/sample-${n}.bg.rgba`);
  const pc = fs.readFileSync(`${DIR}/sample-${n}.piece.rgba`);
  const { w, h, ew, eh, y0 } = meta.dims;

  // gradient magnitude maps (|dx|+|dy| on luminance)
  const gb = new Float32Array(w * h);
  for (let y = 1; y < h - 1; y++) for (let x = 1; x < w - 1; x++) {
    const gx = lum(bg, (y * w + x + 1) * 4) - lum(bg, (y * w + x - 1) * 4);
    const gy = lum(bg, ((y + 1) * w + x) * 4) - lum(bg, ((y - 1) * w + x) * 4);
    gb[y * w + x] = Math.abs(gx) + Math.abs(gy);
  }
  const gp = new Float32Array(ew * eh);
  for (let y = 1; y < eh - 1; y++) for (let x = 1; x < ew - 1; x++) {
    const gx = lum(pc, (y * ew + x + 1) * 4) - lum(pc, (y * ew + x - 1) * 4);
    const gy = lum(pc, ((y + 1) * ew + x) * 4) - lum(pc, ((y - 1) * ew + x) * 4);
    gp[y * ew + x] = Math.abs(gx) + Math.abs(gy);
  }
  const mask = [];
  for (let py = 0; py < eh; py += 2) for (let dx = 0; dx < ew; dx += 2)
    if (pc[(py * ew + dx) * 4 + 3] >= 40) mask.push([dx, py]);

  const ncc = (valsA, valsB) => {
    let sa = 0, sb = 0, sa2 = 0, sb2 = 0, sab = 0, m = 0;
    for (let k = 0; k < valsA.length; k++) {
      const a = valsA[k], b = valsB[k];
      sa += a; sb += b; sa2 += a * a; sb2 += b * b; sab += a * b; m++;
    }
    const cov = sab / m - (sa / m) * (sb / m);
    const sd = Math.sqrt(Math.max(0, sa2 / m - (sa / m) ** 2) * Math.max(0, sb2 / m - (sb / m) ** 2));
    return sd < 1e-6 ? 0 : cov / sd;
  };

  const peaks = { lum: [], grad: [] };
  const yMin = Math.max(0, y0 - 40), yMax = Math.min(h - eh, y0 + 40);
  for (let x = 0; x + ew <= w; x += 2) {
    for (let y = yMin; y <= yMax; y += 2) {
      const lv = [], gv = [], lb = [], gbm = [];
      for (const [dx, py] of mask) {
        const bi = ((y + py) * w + (x + dx)) * 4;
        lv.push((bg[bi] + bg[bi + 1] + bg[bi + 2]) / 3);
        gbm.push(gb[(y + py) * w + (x + dx)]);
        const pi = (py * ew + dx) * 4;
        lv.push(0); // placeholder replaced below
        gv.push(gp[py * ew + dx]);
      }
      // rebuild arrays properly (piece vals constant across positions)
      const pvLum = [], pvGrad = [];
      const bgLum = [], bgGrad = [];
      for (const [dx, py] of mask) {
        const pi = (py * ew + dx) * 4;
        pvLum.push((pc[pi] + pc[pi + 1] + pc[pi + 2]) / 3);
        pvGrad.push(gp[py * ew + dx]);
        const bi = ((y + py) * w + (x + dx)) * 4;
        bgLum.push((bg[bi] + bg[bi + 1] + bg[bi + 2]) / 3);
        bgGrad.push(gb[(y + py) * w + (x + dx)]);
      }
      const sLum = ncc(pvLum, bgLum), sGrad = ncc(pvGrad, bgGrad);
      peaks.lum.push({ x, y, s: Math.round(sLum * 1000) / 1000 });
      peaks.grad.push({ x, y, s: Math.round(sGrad * 1000) / 1000 });
    }
  }
  console.log(`\n===== 样本 ${n}（y0=${y0}）=====`);
  for (const kind of ["lum", "grad"]) {
    const top = [...peaks[kind]].sort((a, b) => b.s - a.s).slice(0, 5);
    console.log(`${kind}: ${top.map((p) => `x=${p.x},y=${p.y}(${p.s})`).join("  ")}`);
  }
}

for (const n of (process.argv[2] ? process.argv.slice(2) : ["4", "5"])) search(n);
