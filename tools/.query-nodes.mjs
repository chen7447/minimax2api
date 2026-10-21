import net from "node:net";
const PIPE = String.raw`\\.\pipe\verge-mihomo`;
const SECRET = "set-your-secret";
const sock = net.connect(PIPE);
let buf = "";
sock.on("connect", () => sock.write(`GET /proxies HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer ${SECRET}\r\nConnection: close\r\n\r\n`));
sock.on("data", (d) => (buf += d));
sock.on("end", () => {
  const raw = buf.slice(buf.indexOf("\r\n\r\n") + 4);
  const a = raw.indexOf("{"), b = raw.lastIndexOf("}");
  const data = JSON.parse(raw.slice(a, b + 1));
  const all = data.proxies["全部节点"]?.all || [];
  const us = all.filter(n => /US|美国|🇺🇸/i.test(n) && !/剩余|到期/.test(n));
  console.log("池子总数:", all.length);
  console.log("美国节点", us.length, "个:");
  us.forEach(n => console.log("  " + n));
});
sock.on("error", (e) => { console.error("管道连接失败:", e.message); process.exit(1); });
