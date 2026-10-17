"use client";

import { Fragment, useEffect, useState, useCallback } from "react";
import { listVendors, createVendor, patchVendor, type VendorDTO } from "@/lib/api";

// Per PRD 2 §6, §51: operator-driven vendor onboarding. No public
// registration flow — this page IS the onboarding mechanism.

const STATUS_COLORS: Record<string, string> = {
  PENDING: "#6b7280",
  ACTIVE: "#15803d",
  INACTIVE: "#b45309",
  BLOCKED: "#dc2626",
};

export default function VendorsPage() {
  const [vendors, setVendors] = useState<VendorDTO[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [saving, setSaving] = useState(false);

  const [form, setForm] = useState({
    business_name: "",
    whatsapp_external_id: "",
    primary_location: "",
    categories: "",
    service_areas: "",
  });

  const load = useCallback(async () => {
    try {
      setVendors(await listVendors());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "failed to load vendors");
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const handleCreate = async () => {
    setSaving(true);
    try {
      const created = await createVendor({
        business_name: form.business_name,
        whatsapp_external_id: form.whatsapp_external_id,
        primary_location: form.primary_location,
        categories: form.categories.split(",").map((c) => c.trim()).filter(Boolean),
        service_areas: form.service_areas.split(",").map((s) => s.trim()).filter(Boolean),
      });
      setForm({ business_name: "", whatsapp_external_id: "", primary_location: "", categories: "", service_areas: "" });
      setShowForm(false);
      await load();
      // Saved, but some categories will never match a buyer request.
      setError(created.warnings?.length ? `Saved ${created.business_name}. Warning: ${created.warnings.join(" ")}` : null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "failed to create vendor");
    } finally {
      setSaving(false);
    }
  };

  const handleActivate = async (id: string) => {
    await patchVendor(id, { status: "ACTIVE" });
    await load();
  };

  // 01a audit: categories, location and service areas decide which requests
  // reach a vendor, so they must be editable here, not only through the API.
  const [editing, setEditing] = useState<string | null>(null);
  const [editForm, setEditForm] = useState({ primary_location: "", categories: "", service_areas: "", status: "ACTIVE" });
  const startEdit = (v: VendorDTO) => {
    setEditing(v.id);
    setEditForm({ primary_location: v.primary_location, categories: v.categories.join(", "), service_areas: (v.service_areas ?? []).join(", "), status: v.status });
  };
  const saveEdit = async () => {
    if (!editing) return;
    setSaving(true);
    try {
      const updated = await patchVendor(editing, {
        primary_location: editForm.primary_location,
        categories: editForm.categories.split(",").map((c) => c.trim()).filter(Boolean),
        service_areas: editForm.service_areas.split(",").map((s) => s.trim()).filter(Boolean),
        status: editForm.status,
      });
      setEditing(null);
      await load();
      setError(updated.warnings?.length ? `Saved ${updated.business_name}. Warning: ${updated.warnings.join(" ")}` : null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "failed to update vendor");
    } finally {
      setSaving(false);
    }
  };

  const inputStyle: React.CSSProperties = { padding: "6px 8px", border: "1px solid #ccc", borderRadius: 4, fontSize: "0.9em", width: "100%" };

  return (
    <div>
      <title>Vendors · Majo</title>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h1>Vendors</h1>
        <button onClick={() => setShowForm(!showForm)} style={{ padding: "8px 16px", background: "#2563eb", color: "white", border: "none", borderRadius: 6, cursor: "pointer" }}>
          {showForm ? "Cancel" : "+ Add Vendor"}
        </button>
      </div>
      <p style={{ color: "#666" }}>Vendors are operator-onboarded (PRD 2 §6) — there is no public registration flow.</p>

      {error && <p style={{ color: "crimson" }}>{error}</p>}

      {showForm && (
        <div style={{ background: "#f9fafb", padding: 16, borderRadius: 8, marginBottom: 20, display: "grid", gap: 10, maxWidth: 500 }}>
          <input style={inputStyle} placeholder="Business name" value={form.business_name} onChange={(e) => setForm({ ...form, business_name: e.target.value })} />
          <input style={inputStyle} placeholder="WhatsApp number (e.g. 0807 111 2222 or +234 807 111 2222)" value={form.whatsapp_external_id} onChange={(e) => setForm({ ...form, whatsapp_external_id: e.target.value })} />
          <input style={inputStyle} placeholder="Primary location (e.g. Benin City)" value={form.primary_location} onChange={(e) => setForm({ ...form, primary_location: e.target.value })} />
          <input style={inputStyle} placeholder="Categories, comma-separated, from: electronics, phones, laptops, gaming, tv, computers, accessories, fashion, furniture, food, services, school" value={form.categories} onChange={(e) => setForm({ ...form, categories: e.target.value })} />
          <input style={inputStyle} placeholder="Service areas, comma-separated (campus areas, halls, gates, nearby areas buyers name)" value={form.service_areas} onChange={(e) => setForm({ ...form, service_areas: e.target.value })} />
          <button onClick={handleCreate} disabled={saving || !form.business_name || !form.whatsapp_external_id} style={{ padding: "8px 16px", background: "#15803d", color: "white", border: "none", borderRadius: 6, cursor: "pointer" }}>
            {saving ? "Creating..." : "Create Vendor"}
          </button>
        </div>
      )}

      <table style={{ width: "100%", borderCollapse: "collapse", marginTop: 16 }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "2px solid #ddd" }}>
            <th style={{ padding: "8px 4px" }}>Status</th>
            <th style={{ padding: "8px 4px" }}>Business</th>
            <th style={{ padding: "8px 4px" }}>Categories</th>
            <th style={{ padding: "8px 4px" }}>Location</th>
            <th style={{ padding: "8px 4px" }}>Requests / Responses</th>
            <th style={{ padding: "8px 4px" }}></th>
          </tr>
        </thead>
        <tbody>
          {vendors.map((v) => (
            <Fragment key={v.id}>
            <tr style={{ borderBottom: "1px solid #eee" }}>
              <td style={{ padding: "8px 4px" }}>
                <span style={{ color: STATUS_COLORS[v.status], fontWeight: 600, fontSize: "0.85em" }}>{v.status}</span>
              </td>
              <td style={{ padding: "8px 4px" }}>
                <strong>{v.business_name}</strong>
                <div style={{ fontSize: "0.8em", color: "#666" }}>{v.whatsapp_external_id}</div>
              </td>
              <td style={{ padding: "8px 4px" }}>{v.categories.join(", ") || "—"}</td>
              <td style={{ padding: "8px 4px" }}>
                {v.primary_location || "—"}
                {(v.service_areas ?? []).length > 0 && <div style={{ fontSize: "0.8em", color: "#666" }}>also: {v.service_areas.join(", ")}</div>}
              </td>
              <td style={{ padding: "8px 4px" }}>{v.requests_received} / {v.responses_received}</td>
              <td style={{ padding: "8px 4px" }}>
                {v.status !== "ACTIVE" && v.status !== "BLOCKED" && (
                  <button onClick={() => handleActivate(v.id)} style={{ padding: "4px 10px", fontSize: "0.85em", cursor: "pointer" }}>
                    Activate
                  </button>
                )}{" "}
                <button onClick={() => startEdit(v)} style={{ padding: "4px 10px", fontSize: "0.85em", cursor: "pointer" }} aria-label={`Edit ${v.business_name}`}>
                  Edit
                </button>
              </td>
            </tr>
            {editing === v.id && (
                  <tr>
                    <td colSpan={6} style={{ background: "#f9fafb", padding: 12 }}>
                      <div style={{ display: "grid", gap: 8, maxWidth: 600 }}>
                        <label style={{ fontSize: "0.85em" }}>
                          Primary location
                          <input style={inputStyle} value={editForm.primary_location} onChange={(e) => setEditForm({ ...editForm, primary_location: e.target.value })} />
                        </label>
                        <label style={{ fontSize: "0.85em" }}>
                          Categories (electronics, phones, laptops, gaming, tv, computers, accessories, fashion, furniture, food, services, school)
                          <input style={inputStyle} value={editForm.categories} onChange={(e) => setEditForm({ ...editForm, categories: e.target.value })} />
                        </label>
                        <label style={{ fontSize: "0.85em" }}>
                          Service areas (other names buyers use for places this vendor serves)
                          <input style={inputStyle} value={editForm.service_areas} onChange={(e) => setEditForm({ ...editForm, service_areas: e.target.value })} />
                        </label>
                        <label style={{ fontSize: "0.85em" }}>
                          Status{" "}
                          <select value={editForm.status} onChange={(e) => setEditForm({ ...editForm, status: e.target.value })}>
                            {["PENDING", "ACTIVE", "INACTIVE", "BLOCKED"].map((s) => (
                              <option key={s}>{s}</option>
                            ))}
                          </select>
                        </label>
                        <div>
                          <button onClick={saveEdit} disabled={saving} style={{ padding: "6px 14px", background: "#15803d", color: "white", border: "none", borderRadius: 6, cursor: "pointer" }}>
                            {saving ? "Saving..." : "Save"}
                          </button>{" "}
                          <button onClick={() => setEditing(null)} style={{ padding: "6px 14px", cursor: "pointer" }}>
                            Cancel
                          </button>
                        </div>
                      </div>
                    </td>
                  </tr>
            )}
            </Fragment>
          ))}
          {vendors.length === 0 && (
            <tr>
              <td colSpan={6} style={{ padding: 0 }}>
                <div className="empty-state" style={{ margin: 12 }}>
                  No vendors yet — use “+ Add Vendor” to onboard the first one.
                </div>
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
