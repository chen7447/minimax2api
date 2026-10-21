// Minimal zero-dep RGBA-buffer -> PNG encoder for captcha debugging
import fs from "node:fs";
import zlib from "node:zlib";

function crc32(buf) {
  let c, table = crc32.table;
  if (!table) {
    table = crc32.table = new Int32Array(256);
    for (let n = 0; n < 256; n++) {
      c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      table[n] = c;
    }
  }
  c = -1;
  for (let i = 0; i < buf.length; i++) c = table[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ -1) >>> 0;
}

const chunk = (type, data) => {
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type), data]);
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
};

export function rgbaToPng(rgba, w, h) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4);
  ihdr[8] = 8; ihdr[9] = 6; // 8-bit RGBA
  const raw = Buffer.alloc((w * 4 + 1) * h);
  for (let y = 0; y < h; y++) {
    raw[y * (w * 4 + 1)] = 0; // filter: none
    rgba.copy ? rgba.copy(raw, y * (w * 4 + 1) + 1, y * w * 4, (y + 1) * w * 4)
              : Buffer.from(rgba.buffer, y * w * 4, w * 4).copy(raw, y * (w * 4 + 1) + 1);
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", zlib.deflateSync(raw, { level: 6 })),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

// CLI: node rgba2png.mjs <in.rgba> <w> <h> <out.png> [scale]
if (import.meta.url === `file://${process.argv[1].replace(/\\/g, "/")}` || process.argv[1].endsWith("rgba2png.mjs")) {
  const [inp, w, h, outp, scale] = process.argv.slice(2);
  const buf = fs.readFileSync(inp);
  const s = Number(scale || 1);
  let src = buf;
  if (s > 1) {
    const W = Number(w), H = Number(h);
    const up = Buffer.alloc(W * s * H * s * 4);
    for (let y = 0; y < H * s; y++) for (let x = 0; x < W * s; x++) {
      const si = (Math.floor(y / s) * W + Math.floor(x / s)) * 4, di = (y * W * s + x) * 4;
      for (let k = 0; k < 4; k++) up[di + k] = buf[si + k];
    }
    src = up;
  }
  fs.writeFileSync(outp, rgbaToPng(src, Number(w) * s, Number(h) * s));
  console.log("written", outp);
}
