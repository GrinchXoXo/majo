"use client";

import { useEffect, useState, useCallback } from "react";
import {
  getUnmatchedResponses,
  getVendorOwnRequests,
  associateVendorResponse,
  type VendorResponseDTO,
  type VendorRequestDTO,
} from "@/lib/api";

// Operator queue for responses that could not be safely auto-associated
// with a VendorRequest (PRD 2.1 §4/§5/§13): ambiguous multi-request
// situations, unknown/invalid MM references, or a vendor with no active
// request at all. Nothing here was ever discarded — this page is where
// an operator resolves it.

function UnmatchedCard({ response, onResolved }: { response: VendorResponseDTO; onResolved: () => void }) {
  const [candidates, setCandidates] = useState<VendorRequestDTO[] | null>(null);
  const [selected, setSelected] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadCandidates = async () => {
    try {
      const vrs = await getVendorOwnRequests(response.vendor_id);
      setCandidates(vrs);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load candidate requests");
    }
  };

  const handleAssociate = async () => {
    if (!selected) return;
    setSaving(true);
    setError(null);
    try {
      await associateVendorResponse(response.id, selected);
      onResolved();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Association failed");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div style={{ border: "1px solid #eee", borderRadius: 8, padding: 14, marginBottom: 12, background: "#fff" }}>
      <div style={{ fontSize: "0.8em", color: "#666", marginBottom: 6 }}>
        Vendor ID: {response.vendor_id} · {new Date(response.created_at).toLocaleString()}
      </div>
      <p style={{ fontStyle: "italic", margin: "0 0 8px" }}>&ldquo;{response.raw_message}&rdquo;</p>
      <div style={{ fontSize: "0.85em", color: "#333", marginBottom: 10 }}>
        Extracted status: <strong>{response.status}</strong>
        {response.price ? ` · ₦${response.price.toLocaleString()}` : ""}
        {response.variant ? ` · ${response.variant}` : ""}
      </div>

      {candidates === null ? (
        <button onClick={loadCandidates} style={{ padding: "5px 12px", fontSize: "0.85em", cursor: "pointer" }}>
          Choose the correct request…
        </button>
      ) : (
        <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
          <select value={selected} onChange={(e) => setSelected(e.target.value)} style={{ padding: "5px 8px", fontSize: "0.85em" }}>
            <option value="">Select a VendorRequest…</option>
            {candidates.map((c) => (
              <option key={c.id} value={c.id}>
                {c.public_reference} — {c.status}
              </option>
            ))}
          </select>
          <button
            onClick={handleAssociate}
            disabled={!selected || saving}
            style={{ padding: "5px 12px", background: "#2563eb", color: "white", border: "none", borderRadius: 6, cursor: "pointer", fontSize: "0.85em" }}
          >
            {saving ? "Saving..." : "Associate"}
          </button>
        </div>
      )}
      {error && <p style={{ color: "crimson", fontSize: "0.8em", marginTop: 6 }}>{error}</p>}
    </div>
  );
}

export default function UnmatchedResponsesPage() {
  const [responses, setResponses] = useState<VendorResponseDTO[]>([]);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setResponses(await getUnmatchedResponses());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load unmatched responses");
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <div>
      <h1>Unmatched Vendor Responses</h1>
      <p style={{ color: "#666" }}>
        Responses that could not be safely auto-associated with a specific request — an ambiguous
        multi-request situation, an unrecognized reference, or no active request at all. Nothing here
        was discarded; each needs a manual association.
      </p>

      {error && <p style={{ color: "crimson" }}>{error}</p>}
      {responses.length === 0 && !error && <p style={{ color: "#666" }}>Nothing to review.</p>}

      {responses.map((r) => (
        <UnmatchedCard key={r.id} response={r} onResolved={load} />
      ))}
    </div>
  );
}
