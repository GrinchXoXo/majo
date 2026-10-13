"use client";

import { useEffect, useState } from "react";
import { getRequestAutonomy, type RequestAutonomy } from "@/lib/api";

// Phase 4C correction: what Majo decided for this request and why (typed
// decision -> policy -> outcome), the questions it asked, and any exception
// that needs the control plane. Read-only; needs the operator token.
export default function AutonomyPanel({ requestId, refreshKey }: { requestId: string; refreshKey?: number }) {
  const [data, setData] = useState<RequestAutonomy | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    getRequestAutonomy(requestId)
      .then((d) => {
        setData(d);
        setErr(null);
      })
      .catch((e) => setErr(e instanceof Error ? e.message : "could not load decisions"));
  }, [requestId, refreshKey]);

  const box: React.CSSProperties = { border: "1px solid #c7d2fe", background: "#eef2ff", borderRadius: 8, padding: 12, marginBottom: 20 };
  if (err) {
    return (
      <section style={box}>
        <strong style={{ fontSize: "0.85em", color: "#3730a3" }}>MAJO (AUTONOMOUS)</strong>
        <p style={{ fontSize: "0.85em", color: "#666" }}>Set the operator token to see Majo&apos;s decisions ({err}).</p>
      </section>
    );
  }
  if (!data) return null;
  const open = data.exceptions.filter((e) => e.status === "OPEN");
  return (
    <section style={box} aria-label="Majo decisions">
      <strong style={{ fontSize: "0.85em", color: "#3730a3" }}>MAJO (AUTONOMOUS) — decisions, questions, exceptions</strong>
      {open.length > 0 ? (
        <p style={{ color: "crimson", fontSize: "0.9em", margin: "6px 0" }}>
          {open.length} open exception(s) need the control plane: {open.map((e) => `${e.class} · ${e.reason}`).join("; ")}
        </p>
      ) : (
        <p style={{ color: "#15803d", fontSize: "0.9em", margin: "6px 0" }}>No exception: Majo handles this request without an operator.</p>
      )}
      {data.clarifications.length > 0 && (
        <div style={{ marginTop: 8 }}>
          <div style={{ fontSize: "0.8em", color: "#555", fontWeight: 600 }}>QUESTIONS ASKED</div>
          {data.clarifications.map((c) => (
            <div key={c.id} style={{ fontSize: "0.85em", margin: "4px 0" }}>
              <span style={{ color: "#3730a3" }}>{c.party.toLowerCase()}</span> · {c.kind}
              {c.field ? ` (${c.field})` : ""}: &ldquo;{c.question.split("\n")[0]}&rdquo; → <strong>{c.status}</strong>
              {c.answer_text ? ` — “${c.answer_text}”` : ""}
            </div>
          ))}
        </div>
      )}
      <div style={{ marginTop: 8 }}>
        <div style={{ fontSize: "0.8em", color: "#555", fontWeight: 600 }}>DECISION LOG</div>
        {data.decisions.length === 0 && <div style={{ fontSize: "0.85em", color: "#666" }}>No decisions yet.</div>}
        {data.decisions.map((d) => (
          <div key={d.id} style={{ fontSize: "0.85em", margin: "4px 0" }}>
            <code>{new Date(d.created_at).toLocaleTimeString()}</code> <strong>{d.decision}</strong> → {d.outcome}
            {d.reason ? ` · ${d.reason}` : ""}
            {d.fallback ? ` · ${d.fallback}` : ""}
            {d.confidence !== null ? ` · confidence ${Math.round(d.confidence * 100)}%` : ""}
            {d.actor !== "SYSTEM" ? ` · by ${d.actor}` : ""}
            {Array.isArray((d.result as { vendor_requests?: string[] }).vendor_requests) &&
              ` · ${((d.result as { vendor_requests?: string[] }).vendor_requests ?? []).join(", ")}`}
          </div>
        ))}
      </div>
    </section>
  );
}
