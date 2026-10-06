"use client";

import { useCallback, useEffect, useState } from "react";
import OperatorToken from "@/components/OperatorToken";
import { ApiError } from "@/lib/api";
import {
  createBatch, getDiscoveryFunnel, getPreferences, getSettings, listBatches, previewBatch, setPreference, splitList,
  type Batch, type Funnel, type PlanReport, type Preference, type Settings,
} from "@/lib/discovery";
import { Counts, FunnelTable, PlanView, PolicyResult, Reasons, cell } from "./components";

// Phase 4B: Discovery & Distribution operator console (staff-only).
// Dry run -> review -> create batch -> send (on the batch page). There is
// no scheduler and no consumer surface here.
export default function DiscoveryPage() {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [prefs, setPrefs] = useState<Preference[]>([]);
  const [batches, setBatches] = useState<Batch[]>([]);
  const [funnel, setFunnel] = useState<Funnel | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  const [category, setCategory] = useState("");
  const [limit, setLimit] = useState("");
  const [recipients, setRecipients] = useState("");
  const [report, setReport] = useState<PlanReport | null>(null);
  const [staff, setStaff] = useState({ recipient: "", status: "OPTED_IN", location: "", interests: "" });

  const load = useCallback(async () => {
    try {
      const [s, p, b, f] = await Promise.all([getSettings(), getPreferences(), listBatches(), getDiscoveryFunnel()]);
      setSettings(s);
      setPrefs(p ?? []);
      setBatches(b ?? []);
      setFunnel(f);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? `${e.code}: ${e.message}` : String(e));
    }
  }, []);
  useEffect(() => {
    load();
  }, [load, tick]);

  const scope = () => ({ category: category.trim() || undefined, limit: limit ? Number(limit) : undefined });
  const run = async (fn: () => Promise<void>) => {
    try {
      await fn();
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? `${e.code}: ${e.message}` : String(e));
    }
  };

  return (
    <div>
      <h1>Discovery (staff-only)</h1>
      <OperatorToken onChange={() => setTick((t) => t + 1)} />
      {error && <p style={{ color: "#b91c1c" }} data-testid="discovery-error">{error}</p>}

      {settings && (
        <section style={{ margin: "12px 0", padding: 12, background: settings.send_allowed ? "#f0fdf4" : "#fef2f2" }} data-testid="discovery-settings">
          <strong>Mode: <span data-testid="discovery-mode">{settings.mode}</span></strong>
          {settings.send_allowed ? " — sends only to allowlisted staff test numbers" : " — no discovery can be sent"}
          <div style={{ fontSize: "0.85em" }}>
            Ranking {settings.ranking_version} · arms {(settings.arms as string[])?.join(", ")} · cap {String(settings.max_deliveries_per_buyer)} per{" "}
            {String(settings.cap_period)} · same-listing dedupe {String(settings.exposure_window)} · window {String(settings.delivery_window || "any time")} ·
            template {settings.template_configured ? "configured" : "not configured"} · enquiry confirmation {String(settings.enquiry_confirmation)}
          </div>
        </section>
      )}

      <section>
        <h2>Consent records</h2>
        <table style={{ borderCollapse: "collapse", fontSize: "0.9em" }} data-testid="preferences">
          <thead>
            <tr>
              {["Number", "Status", "Location", "Interests", "Source", "Since"].map((h) => (
                <th key={h} style={cell}>{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {prefs.map((p) => (
              <tr key={p.id}>
                <td style={cell}>{p.recipient}</td>
                <td style={cell}>{p.status}</td>
                <td style={cell}>{p.location ?? "—"}</td>
                <td style={cell}>{p.interests.join(", ") || "—"}</td>
                <td style={cell}>{p.source}</td>
                <td style={cell}>{new Date(p.captured_at).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <div style={{ marginTop: 8, display: "flex", gap: 6, flexWrap: "wrap" }}>
          <input aria-label="Staff number" placeholder="Staff number (wa_id)" value={staff.recipient} onChange={(e) => setStaff({ ...staff, recipient: e.target.value })} />
          <select aria-label="Consent status" value={staff.status} onChange={(e) => setStaff({ ...staff, status: e.target.value })}>
            <option>OPTED_IN</option>
            <option>OPTED_OUT</option>
          </select>
          <input aria-label="Viewer location" placeholder="Location" value={staff.location} onChange={(e) => setStaff({ ...staff, location: e.target.value })} />
          <input aria-label="Interests" placeholder="Interests (comma)" value={staff.interests} onChange={(e) => setStaff({ ...staff, interests: e.target.value })} />
          <button
            onClick={() =>
              run(async () => {
                await setPreference({ recipient: staff.recipient.trim(), status: staff.status, location: staff.location, interests: splitList(staff.interests) });
                setTick((t) => t + 1);
              })
            }
          >
            Save staff test record
          </button>
        </div>
      </section>

      <section style={{ marginTop: 20 }}>
        <h2>Dry run</h2>
        <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
          <input aria-label="Scope category" placeholder="Category (optional)" value={category} onChange={(e) => setCategory(e.target.value)} />
          <input aria-label="Listings per buyer" placeholder="Limit" value={limit} onChange={(e) => setLimit(e.target.value)} style={{ width: 60 }} />
          <input aria-label="Audience numbers" placeholder="Audience numbers (blank = all consent records)" value={recipients} onChange={(e) => setRecipients(e.target.value)} style={{ width: 320 }} />
          <button data-testid="dry-run" onClick={() => run(async () => setReport(await previewBatch(scope(), splitList(recipients))))}>
            Run dry run
          </button>
          <button
            data-testid="create-batch"
            onClick={() =>
              run(async () => {
                const { batch } = await createBatch(`ops-${Date.now()}`, scope(), splitList(recipients), "dashboard");
                window.location.href = `/discovery/${batch.id}`;
              })
            }
          >
            Create batch (nothing is sent yet)
          </button>
        </div>
        {report && <PlanView report={report} />}
      </section>

      <section style={{ marginTop: 20 }}>
        <h2>Batches</h2>
        <table style={{ borderCollapse: "collapse", fontSize: "0.9em" }} data-testid="batches">
          <thead>
            <tr>
              {["Created", "Status", "Audience", "Eligible", "Excluded", "Deliveries", "Ranking"].map((h) => (
                <th key={h} style={cell}>{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {batches.map((b) => (
              <tr key={b.id}>
                <td style={cell}>
                  <a href={`/discovery/${b.id}`}>{new Date(b.created_at).toLocaleString()}</a>
                </td>
                <td style={cell}>{b.status}</td>
                <td style={cell}>{b.plan.audience_considered}</td>
                <td style={cell}>{b.plan.audience_eligible}</td>
                <td style={cell}>{Object.values(b.plan.excluded_buyers ?? {}).reduce((a, n) => a + n, 0)}</td>
                <td style={cell}>{b.plan.deliveries}</td>
                <td style={cell}>{b.ranking_version}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      {funnel && (
        <section style={{ marginTop: 20 }}>
          <h2>Discovery funnel (all batches)</h2>
          <FunnelTable f={funnel} />
          <Counts title="Exclusions" counts={funnel.exclusions} />
          <Counts title="Sent by arm" counts={funnel.sent_by_arm} />
        </section>
      )}
    </div>
  );
}
