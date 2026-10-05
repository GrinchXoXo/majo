import { getHealth, getReadiness } from "@/lib/api";

// Proves dashboard -> Go connectivity, per Phase 0 acceptance criteria
// (PRD 0 FINAL §3, item 9 / §28 service communication test).
export default async function DashboardPage() {
  let health, readiness, error: string | null = null;

  try {
    [health, readiness] = await Promise.all([getHealth(), getReadiness()]);
  } catch (e) {
    error = e instanceof Error ? e.message : "unknown error";
  }

  return (
    <div>
      <h1>Dashboard</h1>
      <p>Backend connectivity check (Phase 0 smoke test).</p>

      {error && (
        <p style={{ color: "crimson" }}>
          Could not reach backend: {error}. Is the Go server running?
        </p>
      )}

      {health && (
        <section style={{ marginTop: 16 }}>
          <h2>Backend /health</h2>
          <pre>{JSON.stringify(health, null, 2)}</pre>
        </section>
      )}

      {readiness && (
        <section style={{ marginTop: 16 }}>
          <h2>Backend /ready</h2>
          <pre>{JSON.stringify(readiness, null, 2)}</pre>
        </section>
      )}
    </div>
  );
}
