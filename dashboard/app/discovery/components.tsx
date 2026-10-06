"use client";

import type { PlannedCandidate, Funnel, PlanReport } from "@/lib/discovery";
import { FUNNEL_ROWS } from "@/lib/discovery";

const cell: React.CSSProperties = { padding: "4px 8px", borderBottom: "1px solid #eee", verticalAlign: "top", textAlign: "left" };

export function naira(n: number | null | undefined) {
  return n == null ? "—" : "₦" + n.toLocaleString("en-NG");
}

// PolicyResult shows the canonical lifecycle / freshness / geography result
// Discovery used for one listing and one viewer (Phase 4A.5 policy).
export function PolicyResult({ c }: { c: PlannedCandidate }) {
  const e = c.eligibility;
  return (
    <div data-testid={`policy-${c.public_reference}`} style={{ fontSize: "0.85em", lineHeight: 1.5 }}>
      <div>
        <strong>Listing:</strong> {c.public_reference} · {c.product} · {naira(c.price)}
      </div>
      <div>
        <strong>Lifecycle:</strong> Day {e.lifecycle_day} / {e.lifecycle_max_days}
      </div>
      <div>
        <strong>Freshness:</strong> {e.freshness}
        {c.needs_reconfirmation ? " — reconfirm with the seller before promising availability" : ""}
      </div>
      <div>
        <strong>Geo stage:</strong> {e.geo_stage}
      </div>
      <div>
        <strong>Eligible for viewer:</strong> {e.eligible && e.eligible_for_viewer ? "YES" : "NO"}
      </div>
      <div>
        <strong>Reason:</strong> {e.viewer_reason}
      </div>
    </div>
  );
}

export function Reasons({ c }: { c: PlannedCandidate }) {
  return (
    <ul style={{ margin: 0, paddingLeft: 16, fontSize: "0.85em" }}>
      {c.reasons.map((r, i) => (
        <li key={i}>
          {r.points > 0 ? "+" : ""}
          {r.points} {r.detail}
        </li>
      ))}
      <li>
        <strong>= {c.score}</strong>
      </li>
    </ul>
  );
}

export function Counts({ title, counts, testid }: { title: string; counts: Record<string, number> | undefined; testid?: string }) {
  const entries = Object.entries(counts ?? {});
  return (
    <div data-testid={testid}>
      <strong>{title}:</strong>{" "}
      {entries.length === 0 ? "none" : entries.map(([k, v]) => `${k} ${v}`).join(" · ")}
    </div>
  );
}

export function FunnelTable({ f }: { f: Funnel }) {
  return (
    <table style={{ borderCollapse: "collapse", fontSize: "0.9em" }} data-testid="discovery-funnel">
      <tbody>
        {FUNNEL_ROWS.map(([k, label]) => (
          <tr key={k}>
            <td style={cell}>{label}</td>
            <td style={{ ...cell, textAlign: "right" }} data-testid={`funnel-${k}`}>
              {(f[k] as number) ?? 0}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export { cell };

export function PlanView({ report }: { report: PlanReport }) {
  return (
    <div style={{ marginTop: 12 }} data-testid="plan-report">
      <div>
        Supply {report.supply_considered} · eligible listings <strong data-testid="plan-eligible-listings">{report.eligible_listings}</strong> · audience{" "}
        {report.audience_considered} · eligible <strong data-testid="plan-eligible-audience">{report.audience_eligible}</strong> · deliveries{" "}
        <strong data-testid="plan-deliveries">{report.deliveries}</strong> · {report.send_allowed ? "send allowed" : "send NOT allowed"}
      </div>
      <Counts title="Excluded buyers" counts={report.excluded_buyers} testid="plan-excluded-buyers" />
      <Counts title="Excluded listings" counts={report.listing_exclusions} testid="plan-excluded-listings" />
      <table style={{ borderCollapse: "collapse", marginTop: 8, fontSize: "0.9em" }}>
        <thead>
          <tr>
            {["Buyer", "Outcome", "Listing / policy", "Score & reasons"].map((h) => (
              <th key={h} style={cell}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {report.buyers.map((b) =>
            b.planned ? (
              b.candidates.map((c, i) => (
                <tr key={b.buyer_id + c.listing_id}>
                  {i === 0 && (
                    <td style={cell} rowSpan={b.candidates.length}>
                      {b.recipient}
                      <div style={{ fontSize: "0.8em" }}>{b.viewer_location}</div>
                    </td>
                  )}
                  {i === 0 && <td style={cell} rowSpan={b.candidates.length}>planned · {b.arm}</td>}
                  <td style={cell}><PolicyResult c={c} /></td>
                  <td style={cell}><Reasons c={c} /></td>
                </tr>
              ))
            ) : (
              <tr key={b.buyer_id}>
                <td style={cell}>{b.recipient}</td>
                <td style={cell} colSpan={3}>excluded: {b.exclusion}</td>
              </tr>
            ),
          )}
        </tbody>
      </table>
    </div>
  );
}
