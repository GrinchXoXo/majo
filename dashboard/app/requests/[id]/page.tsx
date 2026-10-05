"use client";

import { useEffect, useState, useCallback } from "react";
import { useParams } from "next/navigation";
import VendorResponseCard from "@/components/VendorResponseCard";
import {
  getRequest,
  getRequestEvents,
  patchRequest,
  confirmRequest,
  clarifyRequest,
  getMatches,
  routeToVendors,
  getVendorRequests,
  getOffers,
  selectOffer,
  type BuyerRequestDTO,
  type RequestEventDTO,
  type MatchCandidateDTO,
  type VendorRequestDTO,
  type OfferDTO,
} from "@/lib/api";

// Per PRD 1 §26-27: the operator needs to see the original message, the AI
// extraction, the current (possibly corrected) fields, confidence, and
// event history — and be able to edit fields and confirm.

type EditableFields = {
  category: string;
  product: string;
  variant: string;
  budget_min: string;
  budget_max: string;
  location: string;
  condition: string;
  quantity: string;
  urgency: string;
};

function toEditable(r: BuyerRequestDTO): EditableFields {
  return {
    category: r.category ?? "",
    product: r.product ?? "",
    variant: r.variant ?? "",
    budget_min: r.budget_min?.toString() ?? "",
    budget_max: r.budget_max?.toString() ?? "",
    location: r.location ?? "",
    condition: r.condition ?? "",
    quantity: r.quantity?.toString() ?? "",
    urgency: r.urgency ?? "",
  };
}

export default function RequestDetailPage() {
  const params = useParams();
  const id = params.id as string;

  const [request, setRequest] = useState<BuyerRequestDTO | null>(null);
  const [events, setEvents] = useState<RequestEventDTO[]>([]);
  const [fields, setFields] = useState<EditableFields | null>(null);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [matches, setMatches] = useState<MatchCandidateDTO[]>([]);
  const [selectedVendors, setSelectedVendors] = useState<Set<string>>(new Set());
  const [vendorRequests, setVendorRequests] = useState<VendorRequestDTO[]>([]);
  const [loadingMatches, setLoadingMatches] = useState(false);
  const [offers, setOffers] = useState<OfferDTO[]>([]);
  const [loadingOffers, setLoadingOffers] = useState(false);

  const load = useCallback(async () => {
    try {
      const [req, evts, vrs] = await Promise.all([
        getRequest(id),
        getRequestEvents(id),
        getVendorRequests(id).catch(() => []),
      ]);
      setRequest(req);
      setFields(toEditable(req));
      setEvents(evts);
      setVendorRequests(vrs);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "failed to load request");
    }
  }, [id]);

  useEffect(() => {
    load();
  }, [load]);

  if (error) {
    return <p style={{ color: "crimson" }}>Error: {error}</p>;
  }
  if (!request || !fields) {
    return <p>Loading…</p>;
  }

  const handleChange = (key: keyof EditableFields, value: string) => {
    setFields((prev) => (prev ? { ...prev, [key]: value } : prev));
  };

  const handleSave = async () => {
    setSaving(true);
    setMessage(null);
    try {
      await patchRequest(id, {
        category: fields.category || undefined,
        product: fields.product || undefined,
        variant: fields.variant || undefined,
        budget_min: fields.budget_min ? Number(fields.budget_min) : undefined,
        budget_max: fields.budget_max ? Number(fields.budget_max) : undefined,
        location: fields.location || undefined,
        condition: fields.condition || undefined,
        quantity: fields.quantity ? Number(fields.quantity) : undefined,
        urgency: fields.urgency || undefined,
      });
      setMessage("Saved.");
      await load();
    } catch (e) {
      setMessage(e instanceof Error ? `Save failed: ${e.message}` : "Save failed");
    } finally {
      setSaving(false);
    }
  };

  const handleConfirm = async () => {
    setSaving(true);
    setMessage(null);
    try {
      await confirmRequest(id);
      setMessage("Request confirmed.");
      await load();
    } catch (e) {
      setMessage(e instanceof Error ? `Confirm failed: ${e.message}` : "Confirm failed");
    } finally {
      setSaving(false);
    }
  };

  const handleClarify = async () => {
    setSaving(true);
    setMessage(null);
    try {
      await clarifyRequest(id);
      setMessage("Clarification question sent to buyer.");
      await load();
    } catch (e) {
      setMessage(e instanceof Error ? `Clarify failed: ${e.message}` : "Clarify failed");
    } finally {
      setSaving(false);
    }
  };

  const handleLoadMatches = async () => {
    setLoadingMatches(true);
    setMessage(null);
    try {
      const results = await getMatches(id);
      setMatches(results);
      setSelectedVendors(new Set(results.map((m) => m.vendor_id))); // pre-select all suggested, per PRD 2 §29 operator can then remove
    } catch (e) {
      setMessage(e instanceof Error ? `Failed to load matches: ${e.message}` : "Failed to load matches");
    } finally {
      setLoadingMatches(false);
    }
  };

  const toggleVendorSelection = (vendorId: string) => {
    setSelectedVendors((prev) => {
      const next = new Set(prev);
      if (next.has(vendorId)) next.delete(vendorId);
      else next.add(vendorId);
      return next;
    });
  };

  const handleRoute = async () => {
    if (selectedVendors.size === 0) return;
    setSaving(true);
    setMessage(null);
    try {
      await routeToVendors(id, Array.from(selectedVendors));
      setMessage(`Routed to ${selectedVendors.size} vendor(s).`);
      setMatches([]);
      await load();
    } catch (e) {
      setMessage(e instanceof Error ? `Routing failed: ${e.message}` : "Routing failed");
    } finally {
      setSaving(false);
    }
  };

  const handleBuildOffers = async () => {
    setLoadingOffers(true);
    setMessage(null);
    try {
      setOffers(await getOffers(id));
    } catch (e) {
      setMessage(e instanceof Error ? `Failed to build offers: ${e.message}` : "Failed to build offers");
    } finally {
      setLoadingOffers(false);
    }
  };

  const handleSelectOffer = async (offerId: string) => {
    if (!request) return;
    setSaving(true);
    setMessage(null);
    try {
      const result = await selectOffer(id, offerId, request.buyer_id);
      setMessage(`Offer selected. Transaction ${result.transaction_id} created (payment: ${result.payment_status}).`);
    } catch (e) {
      setMessage(e instanceof Error ? `Selection failed: ${e.message}` : "Selection failed");
    } finally {
      setSaving(false);
    }
  };

  const inputStyle: React.CSSProperties = {
    width: "100%",
    padding: "6px 8px",
    border: "1px solid #ccc",
    borderRadius: 4,
    fontSize: "0.95em",
  };
  const labelStyle: React.CSSProperties = {
    display: "block",
    fontSize: "0.8em",
    color: "#666",
    marginBottom: 4,
  };
  const fieldWrap: React.CSSProperties = { marginBottom: 12 };

  return (
    <div style={{ maxWidth: 900 }}>
      <a href="/requests" style={{ fontSize: "0.9em" }}>&larr; back to requests</a>
      <h1 style={{ marginTop: 8 }}>
        Request{" "}
        <span style={{ fontSize: "0.6em", color: "#666", fontWeight: 400 }}>{request.id}</span>
      </h1>

      <div style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 16 }}>
        <span
          style={{
            padding: "2px 10px",
            borderRadius: 12,
            fontSize: "0.85em",
            fontWeight: 600,
            background: request.status === "CONFIRMED" ? "#dcfce7" : "#fef3c7",
            color: request.status === "CONFIRMED" ? "#15803d" : "#b45309",
          }}
        >
          {request.status}
        </span>
        <span style={{ fontSize: "0.85em", color: "#666" }}>
          confidence: {(request.confidence * 100).toFixed(0)}% · completeness: {request.completeness}
        </span>
      </div>

      <section style={{ marginBottom: 20, padding: 12, background: "#f9fafb", borderRadius: 6 }}>
        <strong style={{ fontSize: "0.85em", color: "#666" }}>ORIGINAL MESSAGE</strong>
        <p style={{ margin: "6px 0 0", fontStyle: "italic" }}>&ldquo;{request.raw_message}&rdquo;</p>
      </section>

      <section style={{ marginBottom: 20 }}>
        <strong style={{ fontSize: "0.85em", color: "#666" }}>AI EXTRACTION (original, never overwritten)</strong>
        <pre
          style={{
            background: "#f3f4f6",
            padding: 12,
            borderRadius: 6,
            fontSize: "0.85em",
            overflowX: "auto",
          }}
        >
          {JSON.stringify(request.extracted_data, null, 2)}
        </pre>
      </section>

      <section style={{ marginBottom: 20 }}>
        <strong style={{ fontSize: "0.85em", color: "#666" }}>CURRENT FIELDS (editable)</strong>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, marginTop: 8 }}>
          <div style={fieldWrap}>
            <label style={labelStyle}>Category</label>
            <input style={inputStyle} value={fields.category} onChange={(e) => handleChange("category", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Product</label>
            <input style={inputStyle} value={fields.product} onChange={(e) => handleChange("product", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Variant</label>
            <input style={inputStyle} value={fields.variant} onChange={(e) => handleChange("variant", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Condition</label>
            <input style={inputStyle} value={fields.condition} onChange={(e) => handleChange("condition", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Budget min (₦)</label>
            <input style={inputStyle} value={fields.budget_min} onChange={(e) => handleChange("budget_min", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Budget max (₦)</label>
            <input style={inputStyle} value={fields.budget_max} onChange={(e) => handleChange("budget_max", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Location</label>
            <input style={inputStyle} value={fields.location} onChange={(e) => handleChange("location", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Quantity</label>
            <input style={inputStyle} value={fields.quantity} onChange={(e) => handleChange("quantity", e.target.value)} />
          </div>
          <div style={fieldWrap}>
            <label style={labelStyle}>Urgency</label>
            <input style={inputStyle} value={fields.urgency} onChange={(e) => handleChange("urgency", e.target.value)} />
          </div>
        </div>
      </section>

      <div style={{ display: "flex", gap: 8, marginBottom: 20 }}>
        <button onClick={handleSave} disabled={saving} style={buttonStyle("#2563eb")}>
          Save Corrections
        </button>
        <button
          onClick={handleConfirm}
          disabled={saving || request.status === "CONFIRMED"}
          style={buttonStyle("#15803d")}
        >
          Confirm Request
        </button>
        <button onClick={handleClarify} disabled={saving} style={buttonStyle("#b45309")}>
          Ask Buyer for Clarification
        </button>
      </div>

      {message && <p style={{ color: message.includes("failed") || message.includes("Failed") ? "crimson" : "#15803d" }}>{message}</p>}

      <section style={{ marginTop: 32, marginBottom: 32 }}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
          <strong style={{ fontSize: "0.85em", color: "#666" }}>SUGGESTED VENDORS</strong>
          <button onClick={handleLoadMatches} disabled={loadingMatches} style={{ padding: "4px 12px", fontSize: "0.85em", cursor: "pointer" }}>
            {loadingMatches ? "Matching..." : "Run Matching"}
          </button>
        </div>

        {matches.length > 0 && (
          <>
            <table style={{ width: "100%", borderCollapse: "collapse", marginTop: 8 }}>
              <thead>
                <tr style={{ textAlign: "left", borderBottom: "1px solid #ddd", fontSize: "0.85em" }}>
                  <th style={{ padding: "6px 4px" }}></th>
                  <th style={{ padding: "6px 4px" }}>Vendor</th>
                  <th style={{ padding: "6px 4px" }}>Score</th>
                  <th style={{ padding: "6px 4px" }}>Match Type</th>
                  <th style={{ padding: "6px 4px" }}>Reasons</th>
                  <th style={{ padding: "6px 4px" }}>In Budget?</th>
                </tr>
              </thead>
              <tbody>
                {matches.map((m) => (
                  <tr key={m.vendor_id} style={{ borderBottom: "1px solid #f0f0f0", fontSize: "0.9em" }}>
                    <td style={{ padding: "6px 4px" }}>
                      <input type="checkbox" checked={selectedVendors.has(m.vendor_id)} onChange={() => toggleVendorSelection(m.vendor_id)} />
                    </td>
                    <td style={{ padding: "6px 4px" }}>{m.vendor_name}</td>
                    <td style={{ padding: "6px 4px", fontWeight: 600 }}>{m.score}</td>
                    <td style={{ padding: "6px 4px" }}>{m.match_type}</td>
                    <td style={{ padding: "6px 4px", color: "#666", fontSize: "0.9em" }}>{m.reasons.join(", ")}</td>
                    <td style={{ padding: "6px 4px" }}>{m.within_budget === null ? "—" : m.within_budget ? "✓" : "✗"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <button onClick={handleRoute} disabled={saving || selectedVendors.size === 0} style={{ marginTop: 10, padding: "8px 16px", background: "#2563eb", color: "white", border: "none", borderRadius: 6, cursor: "pointer" }}>
              Route to {selectedVendors.size} Selected Vendor(s)
            </button>
          </>
        )}
        {matches.length === 0 && <p style={{ color: "#666", fontSize: "0.9em" }}>Click &ldquo;Run Matching&rdquo; to see suggested vendors.</p>}
      </section>

      {vendorRequests.length > 0 && (
        <section style={{ marginBottom: 32 }}>
          <strong style={{ fontSize: "0.85em", color: "#666" }}>VENDOR REQUESTS &amp; RESPONSES</strong>
          <div style={{ marginTop: 8 }}>
            {vendorRequests.map((vr) => (
              <div key={vr.id} style={{ marginBottom: 12 }}>
                <div style={{ fontSize: "0.85em", color: "#666", marginBottom: 4 }}>
                  <strong>{vr.public_reference}</strong> · {vr.status}
                  {vr.match_score !== null && ` · match score ${vr.match_score}`}
                </div>
                {vr.response ? (
                  <VendorResponseCard
                    response={vr.response}
                    onUpdated={(updated) => {
                      setVendorRequests((prev) =>
                        prev.map((v) => (v.id === vr.id ? { ...v, response: updated } : v))
                      );
                    }}
                  />
                ) : (
                  <p style={{ color: "#666", fontSize: "0.85em", fontStyle: "italic" }}>awaiting response</p>
                )}
              </div>
            ))}
          </div>
        </section>
      )}

      <section style={{ marginBottom: 32 }}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
          <strong style={{ fontSize: "0.85em", color: "#666" }}>OFFERS</strong>
          <button onClick={handleBuildOffers} disabled={loadingOffers} style={{ padding: "4px 12px", fontSize: "0.85em", cursor: "pointer" }}>
            {loadingOffers ? "Building..." : "Build / Refresh Offers"}
          </button>
        </div>

        {offers.length === 0 && (
          <p style={{ color: "#666", fontSize: "0.9em" }}>
            No offers yet — only reviewed, classified vendor responses (never UNCLASSIFIED or
            NEEDS_REVIEW) become buyer-facing offers.
          </p>
        )}

        {offers.map((o) => (
          <div key={o.id} style={{ border: "1px solid #eee", borderRadius: 8, padding: 12, marginBottom: 8, background: "#fff" }}>
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
              <div>
                <strong>{o.type}</strong>{" "}
                <span style={{ color: "#666", fontSize: "0.85em" }}>
                  {o.price ? `₦${o.price.toLocaleString()}` : "price on request"} · score {o.ranking_score}
                </span>
              </div>
              <button
                onClick={() => handleSelectOffer(o.id)}
                disabled={saving}
                style={{ padding: "4px 12px", background: "#15803d", color: "white", border: "none", borderRadius: 6, cursor: "pointer", fontSize: "0.85em" }}
              >
                Select for buyer
              </button>
            </div>
            <div style={{ fontSize: "0.8em", color: "#666", marginTop: 4 }}>{o.ranking_reasons.join(" · ")}</div>
          </div>
        ))}
      </section>

      <section style={{ marginTop: 32 }}>
        <strong style={{ fontSize: "0.85em", color: "#666" }}>EVENT HISTORY</strong>
        <ul style={{ listStyle: "none", padding: 0, marginTop: 8 }}>
          {events.map((e, i) => (
            <li
              key={i}
              style={{
                padding: "8px 12px",
                borderLeft: "3px solid #2563eb",
                marginBottom: 6,
                background: "#f9fafb",
                fontSize: "0.85em",
              }}
            >
              <strong>{e.event_type}</strong>{" "}
              <span style={{ color: "#666" }}>
                by {e.actor_type.toLowerCase()} · {new Date(e.created_at).toLocaleString()}
              </span>
            </li>
          ))}
          {events.length === 0 && <li style={{ color: "#666" }}>No events yet.</li>}
        </ul>
      </section>
    </div>
  );
}

function buttonStyle(color: string): React.CSSProperties {
  return {
    padding: "8px 16px",
    background: color,
    color: "white",
    border: "none",
    borderRadius: 6,
    cursor: "pointer",
    fontSize: "0.9em",
  };
}
