import { getHealth, getReadiness, getFunnel, type FunnelReport } from "@/lib/api";

// Proves dashboard -> Go connectivity, per Phase 0 acceptance criteria
// (PRD 0 FINAL §3, item 9 / §28 service communication test).
export default async function DashboardPage() {
  let health, readiness, error: string | null = null;
  let funnel: FunnelReport | null = null;

  try {
    [health, readiness] = await Promise.all([getHealth(), getReadiness()]);
    funnel = await getFunnel(30).catch(() => null);
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

      {funnel && (
        <section style={{ marginTop: 16 }}>
          <h2>Core funnel — last 30 days</h2>
          <p style={{ color: "#666", fontSize: "0.85em", marginTop: 0 }}>
            Counts of each step in the window, from the system-of-record tables (not a cohort). Used to decide
            when later capabilities are justified — see docs/analytics/funnel.md.
          </p>
          <table style={{ borderCollapse: "collapse", fontSize: "0.9em" }}>
            <tbody>
              {funnel.stages.map((s) => (
                <tr key={s.key} style={{ borderBottom: "1px solid #eee" }}>
                  <td style={{ padding: "4px 12px 4px 0" }}>{s.label}</td>
                  <td style={{ padding: "4px 12px", textAlign: "right", fontVariantNumeric: "tabular-nums" }}>
                    <strong>{s.count.toLocaleString()}</strong>
                  </td>
                  <td style={{ padding: "4px 0", color: "#888", fontSize: "0.85em" }}>{s.note}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
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
