// The report cron job: runs every 10 minutes (paas.yaml), prints the
// queue's counts and exits. Its log is the pod log of the run:
//   paas logs --runtime --process report worker-queue <deployment-id>
const QUEUE_URL = (process.env.QUEUE_URL || "").replace(/\/$/, "");

async function main() {
  if (!QUEUE_URL) {
    console.log("QUEUE_URL is not set; nothing to report");
    return;
  }
  const res = await fetch(QUEUE_URL + "/stats");
  if (!res.ok) throw new Error(`stats: HTTP ${res.status}`);
  const s = await res.json();
  console.log(`${new Date().toISOString()} queued=${s.queued} running=${s.running} done=${s.done}`);
}

main().catch((err) => {
  console.error("report failed:", err.message);
  process.exit(1); // the run shows as failed
});
