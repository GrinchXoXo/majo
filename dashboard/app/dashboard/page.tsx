"use client";

import { useEffect, useState } from "react";
import { getHealth, getReadiness, getFunnel, type FunnelReport, type HealthStatus, type SupplyMetric } from "@/lib/api";

function supplyValue(m: SupplyMetric): string {
  if (m.kind === "count") return m.value.toLocaleString();
  const of = m.of ?? 0;
  if (of === 0) return `${m.value} / 0 —`;
  if (m.kind === "rate") return `${m.value} / ${of} (${Math.round((m.value / of) * 100)}%)`;
  return `${(m.value / of).toFixed(1)} (${m.value} / ${of})`;
}

// Proves dashboard -> Go connectivity, per Phase 0 acceptance criteria
// (PRD 0 FINAL §3, item 9 / §28 service communication test). Reuse-audit R1:
// rendered in the browser so the funnel call carries the operator token;
// /health and /ready stay public.
export default function DashboardPage() {
  const [health, setHealth] = useState<HealthStatus | null>(null);
  const [readiness, setReadiness] = useState<HealthStatus | null>(null);
  const [funnel, setFunnel] = useState<FunnelReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [funnelError, setFunnelError] = useState<string | null>(null);

  useEffect(() => {
    Promise.all([getHealth(), getReadiness()])
      .then(([h, r]) => {
        setHealth(h);
        setReadiness(r);
      })
      .catch((e) => setError(e instanceof Error ? e.message : "unknown error"));
    getFunnel(30)
      .then(setFunnel)
      .catch((e) => setFunnelError(e instanceof Error ? e.message : "unknown error"));
  }, []);

  return (
    <div>
      <title>Dashboard · Majo</title>
      <h1>Dashboard</h1>
      <p>Backend connectivity check (Phase 0 smoke test).</p>

      {error && (
        <p style={{ color: "crimson" }}>
          Could not reach backend: {error}. Is the Go server running?
        </p>
      )}

      {funnelError && (
        <p style={{ color: "#b45309" }}>Funnel unavailable ({funnelError}). Set your operator token (top right).</p>
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

      {funnel?.supply_error && (
        <p style={{ color: "crimson", marginTop: 16 }}>Supply metrics unavailable: {funnel.supply_error}</p>
      )}

      {funnel?.supply && funnel.supply.length > 0 && (
        <section style={{ marginTop: 16 }}>
          <h2>Supply — last 30 days</h2>
          <p style={{ color: "#666", fontSize: "0.85em", marginTop: 0 }}>
            Did vendors provide enough current, usable supply? Counted from the signal, listing and vendor tables. Rows
            marked “now” are a snapshot of the current state.
          </p>
          <table style={{ borderCollapse: "collapse", fontSize: "0.9em" }}>
            <tbody>
              {funnel.supply.map((m) => (
                <tr key={m.key} style={{ borderBottom: "1px solid #eee" }}>
                  <td style={{ padding: "4px 12px 4px 0", whiteSpace: "nowrap" }}>
                    {m.label}
                    {m.scope === "now" ? <span style={{ color: "#888", fontSize: "0.8em" }}> (now)</span> : null}
                  </td>
                  <td style={{ padding: "4px 12px", textAlign: "right", fontVariantNumeric: "tabular-nums", whiteSpace: "nowrap" }}>
                    <strong>{supplyValue(m)}</strong>
                  </td>
                  <td style={{ padding: "4px 0", color: "#888", fontSize: "0.85em" }}>{m.note}</td>
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
