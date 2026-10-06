// The web process: a page to enqueue jobs and a small queue API.
//
//   GET  /             jobs and their state
//   POST /jobs         enqueue {"n": <number>} (the form posts here)
//   POST /jobs/claim   worker: take the oldest queued job (204 if none)
//   POST /jobs/:id     worker: report the result {"result": …}
//   GET  /stats        counts by state (the report cron reads it)
//
// The queue lives in this process's memory to keep the example free of
// dependencies; a real app would use Postgres or Redis through an
// environment variable. Processes reach each other through the app's
// public URL (QUEUE_URL), since app pods do not talk to each other
// directly. Requests from the worker and the cron carry QUEUE_TOKEN.
const http = require("http");

const PORT = Number(process.env.PORT) || 8080;
const TOKEN = process.env.QUEUE_TOKEN || "";
const jobs = [];
let nextID = 1;

function json(res, status, body) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

function readBody(req) {
  return new Promise((resolve) => {
    let data = "";
    req.on("data", (c) => {
      data += c;
      if (data.length > 1e5) req.destroy();
    });
    req.on("end", () => resolve(data));
  });
}

function authorized(req) {
  return !TOKEN || req.headers.authorization === "Bearer " + TOKEN;
}

function page() {
  const rows = jobs
    .slice(-30)
    .reverse()
    .map((j) => `<tr><td>#${j.id}</td><td>fib(${j.n})</td><td>${j.state}</td><td>${j.result ?? ""}</td><td>${j.by ?? ""}</td></tr>`)
    .join("");
  return `<!doctype html><meta charset="utf-8"><title>worker-queue</title>
<style>body{font:15px system-ui;max-width:720px;margin:32px auto;padding:0 16px}td,th{padding:4px 10px;text-align:left}</style>
<h1>Kuyruk</h1>
<p>web süreci işi kuyruğa koyar, worker süreci işler, report cron'u her 10 dakikada özet yazar.</p>
<form method="post" action="/jobs"><input name="n" type="number" min="1" max="40" value="30"> <button>fib(n) hesaplat</button></form>
<table><tr><th>İş</th><th>Görev</th><th>Durum</th><th>Sonuç</th><th>İşleyen</th></tr>${rows}</table>
<script>setTimeout(() => location.reload(), 3000)</script>`;
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  if (req.method === "GET" && url.pathname === "/") {
    res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
    return res.end(page());
  }
  if (req.method === "GET" && url.pathname === "/stats") {
    const stats = { queued: 0, running: 0, done: 0 };
    for (const j of jobs) stats[j.state]++;
    return json(res, 200, stats);
  }
  if (req.method === "POST" && url.pathname === "/jobs") {
    const body = await readBody(req);
    const n = Number(new URLSearchParams(body).get("n") ?? JSON.parse(body || "{}").n);
    if (!(n >= 1 && n <= 40)) return json(res, 400, { error: "n must be 1-40" });
    jobs.push({ id: nextID++, n, state: "queued" });
    if (req.headers["content-type"]?.includes("form")) {
      res.writeHead(303, { location: "/" });
      return res.end();
    }
    return json(res, 201, jobs.at(-1));
  }
  if (req.method === "POST" && url.pathname === "/jobs/claim") {
    if (!authorized(req)) return json(res, 401, { error: "bad token" });
    const job = jobs.find((j) => j.state === "queued");
    if (!job) {
      res.writeHead(204);
      return res.end();
    }
    job.state = "running";
    job.by = req.headers["x-worker"] || "";
    return json(res, 200, job);
  }
  const m = url.pathname.match(/^\/jobs\/(\d+)$/);
  if (req.method === "POST" && m) {
    if (!authorized(req)) return json(res, 401, { error: "bad token" });
    const job = jobs.find((j) => j.id === Number(m[1]));
    if (!job) return json(res, 404, { error: "no such job" });
    Object.assign(job, { state: "done", result: JSON.parse((await readBody(req)) || "{}").result });
    return json(res, 200, job);
  }
  json(res, 404, { error: "not found" });
});

process.on("SIGTERM", () => server.close(() => process.exit(0)));
server.listen(PORT, () => console.log(`web listening on :${PORT}`));
