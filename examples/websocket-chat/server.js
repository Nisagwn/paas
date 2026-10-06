// A WebSocket chat room (RFC 6455) on Node's http module, no dependencies.
// GET / serves the page, GET /ws upgrades to a WebSocket; every message is
// sent to everyone connected to this process.
//
// On paas the connection goes browser → Traefik → this pod and stays open
// as long as both ends want; a sleeping deployment is woken by the first
// connection and the activator tunnels it through.
const http = require("http");
const crypto = require("crypto");
const fs = require("fs");
const path = require("path");

const PORT = Number(process.env.PORT) || 8080;
const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";
const MAX_MESSAGE = 64 * 1024;
const page = fs.readFileSync(path.join(__dirname, "public", "index.html"));
const clients = new Set();

const server = http.createServer((req, res) => {
  if (req.url === "/healthz") {
    res.end("ok\n");
    return;
  }
  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end(page);
});

server.on("upgrade", (req, socket) => {
  const key = req.headers["sec-websocket-key"];
  if (req.url !== "/ws" || String(req.headers.upgrade).toLowerCase() !== "websocket" || !key) {
    socket.end("HTTP/1.1 400 Bad Request\r\n\r\n");
    return;
  }
  const accept = crypto.createHash("sha1").update(key + GUID).digest("base64");
  socket.write(
    "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
  );
  socket.setNoDelay(true);
  const client = { socket, buf: Buffer.alloc(0), name: "misafir" };
  clients.add(client);
  announce(`${clients.size} kişi bağlı`);

  socket.on("data", (chunk) => {
    client.buf = Buffer.concat([client.buf, chunk]);
    let frame;
    while ((frame = readFrame(client))) handle(client, frame);
  });
  const drop = () => {
    if (clients.delete(client)) announce(`${clients.size} kişi bağlı`);
  };
  socket.on("close", drop);
  socket.on("error", drop);
});

// readFrame takes one complete frame off client.buf, or returns null.
function readFrame(client) {
  const b = client.buf;
  if (b.length < 2) return null;
  const opcode = b[0] & 0x0f;
  const masked = (b[1] & 0x80) !== 0;
  let len = b[1] & 0x7f;
  let off = 2;
  if (len === 126) {
    if (b.length < 4) return null;
    len = b.readUInt16BE(2);
    off = 4;
  } else if (len === 127) {
    if (b.length < 10) return null;
    len = Number(b.readBigUInt64BE(2));
    off = 10;
  }
  if (len > MAX_MESSAGE || !masked) {
    // Clients must mask; oversized messages end the connection.
    client.socket.destroy();
    client.buf = Buffer.alloc(0);
    return null;
  }
  if (b.length < off + 4 + len) return null;
  const mask = b.subarray(off, off + 4);
  const payload = Buffer.from(b.subarray(off + 4, off + 4 + len));
  for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i % 4];
  client.buf = b.subarray(off + 4 + len);
  return { opcode, payload };
}

function frame(opcode, payload) {
  const len = payload.length;
  let head;
  if (len < 126) {
    head = Buffer.from([0x80 | opcode, len]);
  } else if (len < 65536) {
    head = Buffer.alloc(4);
    head[0] = 0x80 | opcode;
    head[1] = 126;
    head.writeUInt16BE(len, 2);
  } else {
    head = Buffer.alloc(10);
    head[0] = 0x80 | opcode;
    head[1] = 127;
    head.writeBigUInt64BE(BigInt(len), 2);
  }
  return Buffer.concat([head, payload]);
}

function handle(client, { opcode, payload }) {
  switch (opcode) {
    case 0x1: {
      let msg;
      try {
        msg = JSON.parse(payload.toString("utf8"));
      } catch {
        return;
      }
      client.name = String(msg.name || client.name).slice(0, 30);
      const text = String(msg.text || "").slice(0, 500);
      if (text) broadcast({ type: "message", name: client.name, text, at: new Date().toISOString() });
      break;
    }
    case 0x8: // close
      client.socket.end(frame(0x8, Buffer.alloc(0)));
      break;
    case 0x9: // ping
      client.socket.write(frame(0xa, payload));
      break;
  }
}

function broadcast(msg) {
  const data = frame(0x1, Buffer.from(JSON.stringify(msg)));
  for (const c of clients) c.socket.write(data);
}

function announce(text) {
  broadcast({ type: "system", text, host: process.env.HOSTNAME || "", at: new Date().toISOString() });
}

// Pings keep idle connections open through proxies with idle timeouts.
setInterval(() => {
  for (const c of clients) c.socket.write(frame(0x9, Buffer.alloc(0)));
}, 25000).unref();

process.on("SIGTERM", () => {
  for (const c of clients) c.socket.end(frame(0x8, Buffer.alloc(0)));
  server.close(() => process.exit(0));
});

server.listen(PORT, () => {
  console.log(`websocket-chat listening on :${PORT} (process ${process.env.PAAS_PROCESS || "web"})`);
});
