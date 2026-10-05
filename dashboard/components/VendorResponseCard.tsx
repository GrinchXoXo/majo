"use client";

import { useState } from "react";
import { patchVendorResponse, type VendorResponseDTO, type PatchVendorResponseBody } from "@/lib/api";

// Shows every field PRD 2.1 §11 requires: status, review state, all
// structured fields, confidence, extraction method, and the immutable
// raw message — plus inline correction controls for the fields an
// operator is allowed to fix.

const STATUS_COLORS: Record<string, string> = {
  AVAILABLE: "#15803d",
  UNAVAILABLE: "#dc2626",
  CAN_SOURCE: "#2563eb",
  ALTERNATIVE: "#7c3aed",
  UNCLASSIFIED: "#6b7280",
};

const REVIEW_COLORS: Record<string, string> = {
  NEEDS_REVIEW: "#b45309",
  REVIEWED: "#15803d",
  CORRECTED: "#2563eb",
};

type Props = {
  response: VendorResponseDTO;
  vendorName?: string;
  onUpdated?: (updated: VendorResponseDTO) => void;
};

export default function VendorResponseCard({ response, vendorName, onUpdated }: Props) {
  const [editing, setEditing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState({
    status: response.status,
    variant: response.variant ?? "",
    condition: response.condition ?? "",
    price: response.price?.toString() ?? "",
    quantity: response.quantity?.toString() ?? "",
    location: response.location ?? "",
    notes: response.notes ?? "",
  });

  const badge = (text: string, color: string) => (
    <span style={{ padding: "1px 8px", borderRadius: 10, fontSize: "0.78em", fontWeight: 600, color, background: `${color}1a` }}>
      {text}
    </span>
  );

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    try {
      const body: PatchVendorResponseBody = {
        status: form.status,
        variant: form.variant || undefined,
        condition: form.condition || undefined,
        price: form.price ? Number(form.price) : undefined,
        quantity: form.quantity ? Number(form.quantity) : undefined,
        location: form.location || undefined,
        notes: form.notes || undefined,
      };
      const updated = await patchVendorResponse(response.id, body);
      setEditing(false);
      onUpdated?.(updated);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Correction failed");
    } finally {
      setSaving(false);
    }
  };

  const inputStyle: React.CSSProperties = { padding: "4px 6px", border: "1px solid #ccc", borderRadius: 4, fontSize: "0.85em", width: "100%" };
  const labelStyle: React.CSSProperties = { display: "block", fontSize: "0.72em", color: "#666", marginBottom: 2 };

  return (
    <div style={{ border: "1px solid #eee", borderRadius: 8, padding: 12, marginBottom: 10, background: "#fff" }}>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start" }}>
        <div>
          {vendorName && <strong style={{ marginRight: 8 }}>{vendorName}</strong>}
          {badge(response.status, STATUS_COLORS[response.status] ?? "#6b7280")}{" "}
          {badge(response.review_state, REVIEW_COLORS[response.review_state] ?? "#6b7280")}{" "}
          <span style={{ fontSize: "0.78em", color: "#999" }}>
            {response.extraction_method} · confidence {(response.confidence * 100).toFixed(0)}%
          </span>
        </div>
        <button onClick={() => setEditing(!editing)} style={{ fontSize: "0.78em", padding: "3px 10px", cursor: "pointer" }}>
          {editing ? "Cancel" : "Correct"}
        </button>
      </div>

      <p style={{ margin: "8px 0 4px", fontStyle: "italic", color: "#444", fontSize: "0.9em" }}>
        &ldquo;{response.raw_message}&rdquo;
      </p>

      {!editing && (
        <div style={{ fontSize: "0.85em", color: "#333", display: "grid", gridTemplateColumns: "repeat(3, 1fr)", gap: "4px 12px", marginTop: 6 }}>
          <span>Product: {response.product ?? "—"}</span>
          <span>Variant: {response.variant ?? "—"}</span>
          <span>Condition: {response.condition ?? "—"}</span>
          <span>Price: {response.price ? `₦${response.price.toLocaleString()}` : "—"}</span>
          <span>Quantity: {response.quantity ?? "—"}</span>
          <span>Location: {response.location ?? "—"}</span>
          {response.notes && <span style={{ gridColumn: "1 / -1" }}>Notes: {response.notes}</span>}
        </div>
      )}

      {editing && (
        <div style={{ marginTop: 8, background: "#f9fafb", padding: 10, borderRadius: 6 }}>
          <div style={{ display: "grid", gridTemplateColumns: "repeat(3, 1fr)", gap: 8 }}>
            <div>
              <label style={labelStyle}>Status</label>
              <select style={inputStyle} value={form.status} onChange={(e) => setForm({ ...form, status: e.target.value as VendorResponseDTO["status"] })}>
                <option value="AVAILABLE">AVAILABLE</option>
                <option value="UNAVAILABLE">UNAVAILABLE</option>
                <option value="CAN_SOURCE">CAN_SOURCE</option>
                <option value="ALTERNATIVE">ALTERNATIVE</option>
                <option value="UNCLASSIFIED">UNCLASSIFIED</option>
              </select>
            </div>
            <div>
              <label style={labelStyle}>Variant</label>
              <input style={inputStyle} value={form.variant} onChange={(e) => setForm({ ...form, variant: e.target.value })} />
            </div>
            <div>
              <label style={labelStyle}>Condition</label>
              <input style={inputStyle} value={form.condition} onChange={(e) => setForm({ ...form, condition: e.target.value })} />
            </div>
            <div>
              <label style={labelStyle}>Price (₦)</label>
              <input style={inputStyle} value={form.price} onChange={(e) => setForm({ ...form, price: e.target.value })} />
            </div>
            <div>
              <label style={labelStyle}>Quantity</label>
              <input style={inputStyle} value={form.quantity} onChange={(e) => setForm({ ...form, quantity: e.target.value })} />
            </div>
            <div>
              <label style={labelStyle}>Location</label>
              <input style={inputStyle} value={form.location} onChange={(e) => setForm({ ...form, location: e.target.value })} />
            </div>
            <div style={{ gridColumn: "1 / -1" }}>
              <label style={labelStyle}>Notes</label>
              <input style={inputStyle} value={form.notes} onChange={(e) => setForm({ ...form, notes: e.target.value })} />
            </div>
          </div>
          <button onClick={handleSave} disabled={saving} style={{ marginTop: 8, padding: "5px 14px", background: "#2563eb", color: "white", border: "none", borderRadius: 6, cursor: "pointer", fontSize: "0.85em" }}>
            {saving ? "Saving..." : "Save Correction"}
          </button>
          {error && <p style={{ color: "crimson", fontSize: "0.8em", marginTop: 4 }}>{error}</p>}
        </div>
      )}
    </div>
  );
}
