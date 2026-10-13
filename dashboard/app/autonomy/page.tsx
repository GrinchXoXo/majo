"use client";

import { useCallback, useEffect, useState } from "react";
import OperatorToken from "@/components/OperatorToken";
import {
  getAutonomyMetrics,
  getAutonomySettings,
  getExceptions,
  resolveException,
  type AutonomyMetrics,
  type AutonomySettings,
  type Rate,
  type WorkflowException,
} from "@/lib/api";

// Phase 4C correction: the control plane. Majo runs the commerce loop on its
// own; this page shows how autonomous it actually was (raw counts with every
// percentage) and the only work operators are meant to do: exceptions.

function RateTile({ label, rate, goodWhenLow }: { label: string; rate: Rate; goodWhenLow?: boolean }) {
  const pct = rate.pct === null ? "—" : `${rate.pct}%`;
  const bad = rate.pct !== null && (goodWhenLow ? rate.pct > 0 : rate.pct < 100);
  return (
    <div style={{ border: "1px solid #e5e7eb", borderRadius: 8, padding: 12, minWidth: 190 }}>
      <div style={{ fontSize: "0.8em", color: "#555" }}>{label}</div>
      <div style={{ fontSize: "1.5em", fontWeight: 600, color: bad ? "#b45309" : "#111" }}>{pct}</div>
      <div style={{ fontSize: "0.8em", color: "#666" }}>
        {rate.n} of {rate.of}
      </div>
    </div>
  );
}

function Counts({ title, data }: { title: string; data: Record<string, number> }) {
  const entries = Object.entries(data).sort((a, b) => b[1] - a[1]);
  return (
    <div style={{ minWidth: 240 }}>
      <div style={{ fontSize: "0.8em", color: "#555", fontWeight: 600 }}>{title}</div>
      {entries.length === 0 && <div style={{ fontSize: "0.85em", color: "#666" }}>none</div>}
      {entries.map(([k, v]) => (
        <div key={k} style={{ fontSize: "0.85em" }}>
          {k}: <strong>{v}</strong>
        </div>
      ))}
    </div>
  );
}

export default function AutonomyPage() {
  const [settings, setSettings] = useState<AutonomySettings | null>(null);
  const [metrics, setMetrics] = useState<AutonomyMetrics | null>(null);
  const [exceptions, setExceptions] = useState<WorkflowException[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [notes, setNotes] = useState<Record<string, string>>({});

  const load = useCallback(async () => {
    try {
      const [s, m, e] = await Promise.all([getAutonomySettings(), getAutonomyMetrics(), getExceptions("OPEN")]);
      setSettings(s);
      setMetrics(m);
      setExceptions(e ?? []);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : "could not load");
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const resolve = async (id: string) => {
    const note = (notes[id] ?? "").trim();
    if (!note) return;
    await resolveException(id, note);
    await load();
  };

  return (
    <div style={{ maxWidth: 1100 }}>
      <h1>Autonomy</h1>
      <p style={{ color: "#555", maxWidth: 760 }}>
        Majo is autonomous by default: it confirms clear requests, asks buyers and vendors what it needs, routes by policy and recovers
        failed deliveries. Operators are the control plane — they handle the exceptions below and can override any step.
      </p>
      <OperatorToken onChange={load} />
      {err && <p style={{ color: "crimson" }}>{err}</p>}

      {settings && (
        <section style={{ margin: "16px 0" }}>
          <strong style={{ fontSize: "0.85em", color: "#666" }}>FLAGS ({settings.policy_version})</strong>
          <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginTop: 6 }}>
            {Object.entries(settings.flags).map(([k, v]) => (
              <span key={k} style={{ fontSize: "0.8em", padding: "2px 8px", borderRadius: 12, background: v ? "#dcfce7" : "#f3f4f6", color: v ? "#15803d" : "#555" }}>
                {k} {v ? "ON" : "OFF"}
              </span>
            ))}
          </div>
        </section>
      )}

      {metrics && (
        <>
          <section style={{ margin: "16px 0" }}>
            <strong style={{ fontSize: "0.85em", color: "#666" }}>AUTONOMY ({metrics.total_workflows} workflows)</strong>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 12, marginTop: 8 }}>
              <RateTile label="Autonomous request handling" rate={metrics.autonomous_request_handling} />
              <RateTile label="Autonomous completion (transactions)" rate={metrics.autonomous_completion} />
              <RateTile label="Manual routing" rate={metrics.manual_routing} goodWhenLow />
              <RateTile label="Manual confirmation" rate={metrics.manual_confirmation} goodWhenLow />
              <RateTile label="Operator intervention" rate={metrics.operator_intervention} goodWhenLow />
              <RateTile label="Questions answered (legacy)" rate={metrics.clarification_resolution} />
              <RateTile label="Automatic recovery (retries)" rate={metrics.automatic_recovery} />
            </div>
          </section>
          {metrics.workflow && (
            <section style={{ margin: "16px 0" }} aria-label="Workflow level">
              <strong style={{ fontSize: "0.85em", color: "#666" }}>
                WORKFLOWS — denominator: {metrics.workflow.started} buyer requests started
              </strong>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 12, marginTop: 8 }}>
                <RateTile label="Completed (transaction)" rate={metrics.workflow.completed} />
                <RateTile label="Completed autonomously" rate={metrics.workflow.completed_autonomously} />
                <RateTile label="Needed a question" rate={metrics.workflow.requiring_clarification} goodWhenLow />
                <RateTile label="Escalated to operator" rate={metrics.workflow.escalated_to_operator} goodWhenLow />
                <RateTile label="Operator performed a step" rate={metrics.workflow.operator_performed_step} goodWhenLow />
                <RateTile label="Ended without an offer" rate={metrics.workflow.ended_without_offer} goodWhenLow />
                <RateTile label="Abandoned (question expired)" rate={metrics.workflow.abandoned} goodWhenLow />
                <RateTile label="Still pending" rate={metrics.workflow.pending} goodWhenLow />
              </div>
            </section>
          )}
          {metrics.questions && (
            <section style={{ margin: "16px 0" }} aria-label="Question level">
              <strong style={{ fontSize: "0.85em", color: "#666" }}>
                QUESTIONS — denominator: {metrics.questions.delivered} questions delivered ({metrics.questions.messages_sent} messages incl. re-asks)
              </strong>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 12, marginTop: 8 }}>
                <RateTile label="Answered" rate={metrics.questions.answered} />
                <RateTile label="Resolved (not asked again)" rate={metrics.questions.resolved} />
                <RateTile label="Re-asked" rate={metrics.questions.re_asked} goodWhenLow />
                <RateTile label="Expired / gave up" rate={metrics.questions.expired_or_gave_up} goodWhenLow />
              </div>
            </section>
          )}
          {metrics.recovery && metrics.delivery && (
            <section style={{ display: "flex", flexWrap: "wrap", gap: 24, margin: "16px 0" }} aria-label="Recovery and delivery">
              <RateTile label="Delivery retries that recovered" rate={metrics.recovery.delivery_retries} />
              <RateTile label="Re-routes that led to an offer" rate={metrics.recovery.reroute_led_to_offer} />
              <Counts title="REQUEST RECOVERY DECISIONS" data={metrics.recovery.recovery_decisions} />
              <Counts
                title="DELIVERY"
                data={{
                  "vendor requests expired": metrics.recovery.vendor_requests_expired,
                  "duplicate actions prevented": metrics.recovery.duplicate_actions_prevented,
                  "policy-blocked sends": metrics.delivery.policy_blocked,
                  "notices queued": metrics.delivery.notices_queued,
                  "notices delivered late": metrics.delivery.notices_delivered,
                  "notices still pending": metrics.delivery.notices_pending,
                  "provider-reported failures": metrics.delivery.provider_failures_reported_async,
                }}
              />
            </section>
          )}
          <section style={{ display: "flex", flexWrap: "wrap", gap: 24, margin: "16px 0" }}>
            <Counts title="WORKFLOWS BY CLASS" data={metrics.workflows_by_class} />
            <Counts title="INTERVENTIONS BY REASON" data={metrics.interventions_by_reason} />
            <Counts title="FAILURES BY CATEGORY" data={metrics.failures_by_category} />
            <Counts title="EXCEPTIONS BY REASON" data={metrics.exceptions_by_reason} />
          </section>
        </>
      )}

      <section style={{ margin: "16px 0" }} aria-label="Open exceptions">
        <strong style={{ fontSize: "0.85em", color: "#666" }}>OPEN EXCEPTIONS ({exceptions.length})</strong>
        {exceptions.length === 0 && <p style={{ color: "#15803d" }}>Nothing needs an operator.</p>}
        {exceptions.map((e) => (
          <div key={e.id} style={{ border: "1px solid #fecaca", background: "#fff7f7", borderRadius: 8, padding: 12, marginTop: 8 }}>
            <div>
              <strong>{e.class}</strong> · {e.reason} · {e.subject_type}{" "}
              {e.buyer_request_id && (
                <a href={`/requests/${e.buyer_request_id}`} style={{ fontSize: "0.9em" }}>
                  open request
                </a>
              )}
            </div>
            <div style={{ fontSize: "0.85em", color: "#555" }}>
              {String((e.detail as { why?: string }).why ?? (e.detail as { error?: string }).error ?? "")}
            </div>
            <div style={{ display: "flex", gap: 8, marginTop: 6 }}>
              <input
                aria-label={`Resolution for ${e.id}`}
                placeholder="What did you do?"
                value={notes[e.id] ?? ""}
                onChange={(ev) => setNotes({ ...notes, [e.id]: ev.target.value })}
                style={{ flex: 1, padding: 6 }}
              />
              <button onClick={() => resolve(e.id)} style={{ padding: "6px 12px" }}>
                Resolve
              </button>
            </div>
          </div>
        ))}
      </section>
    </div>
  );
}
