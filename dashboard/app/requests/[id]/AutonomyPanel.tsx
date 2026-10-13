"use client";

import { useCallback, useEffect, useState } from "react";
import { getRequestAutonomy, pauseRequest, resumeRequest, type RequestAutonomy } from "@/lib/api";

// Phase 4C correction + hardening: what Majo decided for this request and
// why (typed decision -> policy -> outcome), the questions it asked, what it
// sent (and whether it was delivered, failed, or is waiting for a compliant
// route), any exception that needs the control plane, and the operator's
// pause / resume. Needs the operator token.
export default function AutonomyPanel({ requestId, refreshKey }: { requestId: string; refreshKey?: number }) {
  const [data, setData] = useState<RequestAutonomy | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [reason, setReason] = useState("");
  const [actionErr, setActionErr] = useState<string | null>(null);

  const load = useCallback(() => {
    getRequestAutonomy(requestId)
      .then((d) => {
        setData(d);
        setErr(null);
      })
      .catch((e) => setErr(e instanceof Error ? e.message : "could not load decisions"));
  }, [requestId]);

  useEffect(() => {
    load();
  }, [load, refreshKey]);

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
  const activeHold = (data.holds ?? []).find((h) => !h.released_at);
  const pending = (data.pending_notices ?? []).filter((n) => n.status === "PENDING");

  const act = async (pause: boolean) => {
    setActionErr(null);
    if (!reason.trim()) {
      setActionErr("A reason is required (it is audited).");
      return;
    }
    try {
      if (pause) await pauseRequest(requestId, reason.trim());
      else await resumeRequest(requestId, reason.trim());
      setReason("");
      load();
    } catch (e) {
      setActionErr(e instanceof Error ? e.message : "failed");
    }
  };

  return (
    <section style={box} aria-label="Majo decisions">
      <strong style={{ fontSize: "0.85em", color: "#3730a3" }}>MAJO (AUTONOMOUS) — decisions, messages, questions, exceptions</strong>
      {activeHold ? (
        <p style={{ color: "#b45309", fontSize: "0.9em", margin: "6px 0" }} data-testid="hold-status">
          PAUSED by {activeHold.held_by}: {activeHold.reason}. Majo takes no automatic step for this request until it is resumed.
        </p>
      ) : open.length > 0 ? (
        <p style={{ color: "crimson", fontSize: "0.9em", margin: "6px 0" }}>
          {open.length} open exception(s) need the control plane: {open.map((e) => `${e.class} · ${e.reason}`).join("; ")}
        </p>
      ) : (
        <p style={{ color: "#15803d", fontSize: "0.9em", margin: "6px 0" }}>No exception: Majo handles this request without an operator.</p>
      )}
      <div style={{ display: "flex", gap: 8, margin: "6px 0" }}>
        <input
          aria-label="Pause or resume reason"
          placeholder="Reason (required)"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          style={{ flex: 1, padding: 6 }}
        />
        {activeHold ? (
          <button onClick={() => act(false)} style={{ padding: "6px 12px" }}>
            Resume automation
          </button>
        ) : (
          <button onClick={() => act(true)} style={{ padding: "6px 12px" }}>
            Pause automation
          </button>
        )}
      </div>
      {actionErr && <p style={{ color: "crimson", fontSize: "0.85em" }}>{actionErr}</p>}
      {pending.length > 0 && (
        <div style={{ marginTop: 8 }}>
          <div style={{ fontSize: "0.8em", color: "#555", fontWeight: 600 }}>WAITING TO BE DELIVERED</div>
          {pending.map((n) => (
            <div key={n.id} style={{ fontSize: "0.85em", margin: "4px 0" }}>
              {n.kind} · blocked by {n.failure_class}
              {n.failure_code ? ` (${n.failure_code})` : ""} · {n.attempts} attempt(s) — goes out when the buyer next writes
            </div>
          ))}
        </div>
      )}
      {(data.messages ?? []).length > 0 && (
        <div style={{ marginTop: 8 }}>
          <div style={{ fontSize: "0.8em", color: "#555", fontWeight: 600 }}>MESSAGES SENT</div>
          {(data.messages ?? []).map((m) => (
            <div key={m.message_id} style={{ fontSize: "0.85em", margin: "4px 0" }}>
              <code>{new Date(m.sent_at).toLocaleTimeString()}</code> {m.kind} →{" "}
              <strong style={{ color: m.delivery_status === "failed" ? "crimson" : "#111" }}>{m.delivery_status}</strong>
              {m.error ? ` · ${m.error}` : ""}
            </div>
          ))}
        </div>
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
