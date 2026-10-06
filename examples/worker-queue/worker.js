// The worker process: no port, runs until stopped. It claims jobs from the
// web process (QUEUE_URL, e.g. https://worker-queue.<domain>), computes
// them and reports the result. On SIGTERM (scale down, a new production
// deployment, a rollback) it finishes the current job and exits.
const QUEUE_URL = (process.env.QUEUE_URL || "").replace(/\/$/, "");
const TOKEN = process.env.QUEUE_TOKEN || "";
const ME = process.env.HOSTNAME || "worker";
let stopping = false;

process.on("SIGTERM", () => {
  console.log("SIGTERM: finishing the current job, then exiting");
  stopping = true;
});

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const fib = (n) => (n < 2 ? n : fib(n - 1) + fib(n - 2));

async function call(path, body) {
  const res = await fetch(QUEUE_URL + path, {
    method: "POST",
    headers: { "content-type": "application/json", authorization: "Bearer " + TOKEN, "x-worker": ME },
    body: JSON.stringify(body ?? {}),
  });
  if (res.status === 204) return null;
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
  return res.json();
}

async function main() {
  console.log(`worker ${ME} started (deployment ${process.env.PAAS_COMMIT_SHA?.slice(0, 7)}, process ${process.env.PAAS_PROCESS})`);
  if (!QUEUE_URL) {
    // Stay up: a worker that exits would be restarted in a loop.
    console.log("QUEUE_URL is not set; run: paas env set worker-queue QUEUE_URL=https://worker-queue.<domain>");
  }
  while (!stopping) {
    if (!QUEUE_URL) {
      await sleep(5000);
      continue;
    }
    try {
      const job = await call("/jobs/claim");
      if (!job) {
        await sleep(1000);
        continue;
      }
      const result = fib(job.n);
      await call(`/jobs/${job.id}`, { result });
      console.log(`job #${job.id}: fib(${job.n}) = ${result}`);
    } catch (err) {
      console.log("queue unavailable:", err.message);
      await sleep(3000);
    }
  }
  process.exit(0);
}

main();
