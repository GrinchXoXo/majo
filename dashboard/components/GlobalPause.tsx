"use client";

// Pilot batch item 4: one switch that stops every automatic step (confirming,
// routing, questions, offer delivery, notices) without a restart. Inbound
// messages are still recorded; operator overrides still work. Resuming
// re-runs whatever was left mid-flow.

import { useCallback, useEffect, useState } from "react";
import { getPauseStatus, pauseAllAutomation, resumeAllAutomation, type PauseStatus } from "@/lib/api";

function when(iso?: string) {
  return iso ? new Date(iso).toLocaleString() : "";
}

// PauseBanner: shown on every page while automation is paused.
export function PauseBanner() {
  const [status, setStatus] = useState<PauseStatus | null>(null);
  useEffect(() => {
    let live = true;
    const read = () =>
      getPauseStatus()
        .then((s) => live && setStatus(s))
        .catch(() => live && setStatus(null)); // no token / not configured: show nothing
    read();
    const t = setInterval(read, 30000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, []);
  if (!status?.paused || !status.active) return null;
  return (
    <div role="alert" data-testid="pause-banner" style={{ background: "#b91c1c", color: "#fff", padding: "8px 20px", fontWeight: 600 }}>
      ALL AUTOMATION IS PAUSED — by {status.active.paused_by}, {when(status.active.paused_at)}: “{status.active.reason}”.{" "}
      <a href="/autonomy" style={{ color: "#fff", textDecoration: "underline" }}>
        Resume on the Autonomy page
      </a>
    </div>
  );
}

// GlobalPausePanel: the control itself (Autonomy page).
export default function GlobalPausePanel() {
  const [status, setStatus] = useState<PauseStatus | null>(null);
  const [reason, setReason] = useState("");
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setStatus(await getPauseStatus());
      setMsg(null);
    } catch (e) {
      setMsg(e instanceof Error ? e.message : "could not read the pause state");
    }
  }, []);
  useEffect(() => {
    load();
  }, [load]);

  const act = async () => {
    const r = reason.trim();
    if (!r || !status) return;
    setBusy(true);
    try {
      if (status.paused) {
        await resumeAllAutomation(r);
        setMsg("Automation resumed. Majo is picking up where it stopped.");
      } else {
        await pauseAllAutomation(r);
        setMsg("All automation paused.");
      }
      setReason("");
      setArmed(false);
      await load();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  if (!status) return msg ? <p style={{ color: "crimson" }}>{msg}</p> : null;
  const paused = status.paused && status.active;
  return (
    <section
      aria-label="Global pause"
      style={{ border: `2px solid ${paused ? "#b91c1c" : "#e5e7eb"}`, borderRadius: 8, padding: 12, margin: "16px 0", background: paused ? "#fef2f2" : "#fff" }}
    >
      <strong style={{ fontSize: "0.85em", color: "#666" }}>GLOBAL PAUSE</strong>
      {paused ? (
        <p style={{ margin: "6px 0", color: "#b91c1c", fontWeight: 600 }} data-testid="pause-state">
          Paused by {status.active!.paused_by} at {when(status.active!.paused_at)} — “{status.active!.reason}”. Majo is taking no automatic step.
        </p>
      ) : (
        <p style={{ margin: "6px 0", color: "#15803d" }} data-testid="pause-state">
          Running. Pausing stops every automatic step (confirming, routing, questions, offer delivery) until you resume.
        </p>
      )}
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <input
          aria-label="Reason"
          placeholder={paused ? "Why is it safe to resume?" : "Why pause everything?"}
          value={reason}
          onChange={(e) => {
            setReason(e.target.value);
            setArmed(false);
          }}
          style={{ flex: 1, minWidth: 240, padding: 6 }}
        />
        {!armed ? (
          <button disabled={!reason.trim() || busy} onClick={() => setArmed(true)} style={{ padding: "6px 12px" }}>
            {paused ? "Resume all automation" : "Pause all automation"}
          </button>
        ) : (
          <button
            disabled={busy}
            onClick={act}
            style={{ padding: "6px 12px", background: paused ? "#15803d" : "#b91c1c", color: "#fff", border: 0, borderRadius: 4 }}
          >
            {paused ? "Confirm resume" : "Confirm pause"}
          </button>
        )}
      </div>
      {msg && <p style={{ fontSize: "0.85em", color: "#555" }}>{msg}</p>}
      {status.recent.length > 0 && (
        <details style={{ marginTop: 8, fontSize: "0.85em" }}>
          <summary>History ({status.recent.length})</summary>
          {status.recent.map((p) => (
            <div key={p.id}>
              {when(p.paused_at)} paused by {p.paused_by} (“{p.reason}”)
              {p.released_at ? ` — resumed ${when(p.released_at)} by ${p.released_by} (“${p.release_reason}”)` : " — still paused"}
            </div>
          ))}
        </details>
      )}
    </section>
  );
}
