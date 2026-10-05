// Rendered on every request, so the response shows the running deployment.
export const dynamic = "force-dynamic";

export default function Home() {
  const commit = (process.env.PAAS_COMMIT_SHA || "local").slice(0, 7);
  return (
    <main>
      <h1>hello from next.js</h1>
      <p>commit {commit}</p>
    </main>
  );
}
