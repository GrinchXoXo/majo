"use client";

import { useCallback, useEffect, useState } from "react";
import OperatorToken from "@/components/OperatorToken";
import { ApiError } from "@/lib/api";
import { getBatch, getDelivery, getDiscoveryFunnel, sendBatch, type Batch, type Delivery, type DeliveryDetail, type Funnel } from "@/lib/discovery";
import { Counts, FunnelTable, PlanView, PolicyResult, Reasons, cell } from "../components";

// Batch detail: plan (dry-run report), deliveries with the canonical policy
// result per listing, the operator send action, and per-delivery attribution.
export default function BatchPage({ params }: { params: { id: string } }) {
  const [batch, setBatch] = useState<Batch | null>(null);
  const [deliveries, setDeliveries] = useState<Delivery[]>([]);
  const [funnel, setFunnel] = useState<Funnel | null>(null);
  const [detail, setDetail] = useState<Record<string, DeliveryDetail>>({});
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  const load = useCallback(async () => {
    try {
      const r = await getBatch(params.id);
      setBatch(r.batch);
      setDeliveries(r.deliveries ?? []);
      setFunnel(await getDiscoveryFunnel(params.id));
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? `${e.code}: ${e.message}` : String(e));
    }
  }, [params.id]);
  useEffect(() => {
    load();
  }, [load, tick]);

  const send = async () => {
    if (!confirm("Send this batch to its staff test recipients now?")) return;
    try {
      const r = await sendBatch(params.id);
      setNotice(r.already_sent ? `Already sent (${r.status})` : `Sent ${r.sent}, failed ${r.failed}, skipped ${Object.values(r.skipped).reduce((a, n) => a + n, 0)} → ${r.status}`);
      setTick((t) => t + 1);
    } catch (e) {
      setError(e instanceof ApiError ? `${e.code}: ${e.message}` : String(e));
    }
  };
  const open = async (id: string) => {
    try {
      const d = await getDelivery(id);
      setDetail((m) => ({ ...m, [id]: d }));
    } catch (e) {
      setError(e instanceof ApiError ? `${e.code}: ${e.message}` : String(e));
    }
  };

  return (
    <div>
      <p>
        <a href="/discovery">← Discovery</a>
      </p>
      <OperatorToken onChange={() => setTick((t) => t + 1)} />
      {error && <p style={{ color: "#b91c1c" }} data-testid="batch-error">{error}</p>}
      {notice && <p style={{ color: "#15803d" }} data-testid="batch-notice">{notice}</p>}
      {batch && (
        <>
          <h1>
            Batch <span data-testid="batch-status">{batch.status}</span>
          </h1>
          <div style={{ fontSize: "0.85em" }}>
            {batch.id} · mode {batch.mode} · ranking {batch.ranking_version} · {batch.presentation_version} · arms {batch.arms.join(", ")}
          </div>
          {batch.status === "PLANNED" && (
            <button data-testid="send-batch" onClick={send} style={{ marginTop: 8 }}>
              Send to staff test recipients
            </button>
          )}
          <h2>Plan</h2>
          <PlanView report={batch.plan} />
          <h2>Deliveries</h2>
          <table style={{ borderCollapse: "collapse", fontSize: "0.9em" }} data-testid="deliveries">
            <thead>
              <tr>
                {["Buyer", "Arm", "Status", "Listings (policy, score, reasons)", "Interaction", "Attribution"].map((h) => (
                  <th key={h} style={cell}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {deliveries.map((d) => (
                <tr key={d.id} data-testid={`delivery-${d.recipient}`}>
                  <td style={cell}>{d.recipient}</td>
                  <td style={cell}>{d.arm}</td>
                  <td style={cell}>
                    {d.status}
                    {d.presentation ? ` (${d.presentation})` : ""}
                    {d.skip_reason ? ` — ${d.skip_reason}` : ""}
                    {d.error ? <div style={{ color: "#b91c1c", fontSize: "0.8em" }}>{d.error}</div> : null}
                  </td>
                  <td style={cell}>
                    {d.candidates.map((c) => (
                      <div key={c.listing_id} style={{ marginBottom: 6 }}>
                        <PolicyResult c={c} />
                        <Reasons c={c} />
                      </div>
                    ))}
                  </td>
                  <td style={cell}>
                    {d.interaction_count} {d.last_action ? `(last: ${d.last_action})` : ""}
                  </td>
                  <td style={cell}>
                    {detail[d.id] ? <AttributionView d={detail[d.id]} /> : <button onClick={() => open(d.id)}>Show</button>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {funnel && (
            <>
              <h2>Funnel for this batch</h2>
              <FunnelTable f={funnel} />
              <Counts title="Exclusions" counts={funnel.exclusions} />
            </>
          )}
        </>
      )}
    </div>
  );
}

function AttributionView({ d }: { d: DeliveryDetail }) {
  const a = d.attribution;
  return (
    <div style={{ fontSize: "0.8em" }} data-testid="attribution">
      <div>Permitted now: {d.permitted_now.permitted ? "yes" : `no (${d.permitted_now.reasons.join(", ")})`}</div>
      <div>
        Messages {a.messages.length} · delivered {a.messages.filter((m) => m.delivered_at).length} · read {a.messages.filter((m) => m.read_at).length}
      </div>
      <div>Taps: {a.interactions.map((i) => `${i.action ?? "?"}${i.outcome === "UNRESOLVED" ? `✗${i.reason}` : ""}`).join(", ") || "none"}</div>
      <div>
        Requests {a.requests.map((r) => r.status).join(", ") || "0"} → offers {a.offers.length} → selections {a.selections.length} → transactions{" "}
        {a.transactions.map((t) => `${t.status}/${t.payment_status}`).join(", ") || "0"}
      </div>
    </div>
  );
}
