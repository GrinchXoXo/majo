"use client";

import { useEffect, useState, useCallback } from "react";
import { searchSupplyListings, activateListing, type SupplyListingDTO } from "@/lib/api";

// Marketplace discovery foundation (PRD 3 §14/§33): browse structured
// supply directly, independent of any specific BuyerRequest.

const STATUS_COLORS: Record<string, string> = {
  DRAFT: "#6b7280",
  ACTIVE: "#15803d",
  PAUSED: "#b45309",
  SOLD: "#7c3aed",
  UNAVAILABLE: "#dc2626",
  EXPIRED: "#9ca3af",
  ARCHIVED: "#9ca3af",
};

export default function SupplyListingsPage() {
  const [listings, setListings] = useState<SupplyListingDTO[]>([]);
  const [statusFilter, setStatusFilter] = useState("");
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const params: Record<string, string> = {};
      if (statusFilter) params.status = statusFilter;
      setListings(await searchSupplyListings(params));
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load listings");
    }
  }, [statusFilter]);

  useEffect(() => {
    load();
  }, [load]);

  const handleActivate = async (id: string) => {
    await activateListing(id);
    await load();
  };

  return (
    <div>
      <h1>Supply Listings</h1>
      <p style={{ color: "#666" }}>
        Durable marketplace supply — distinct from raw vendor SupplySignals. Discoverable
        independent of any specific buyer request.
      </p>

      <div style={{ marginBottom: 12 }}>
        <select value={statusFilter} onChange={(e) => setStatusFilter(e.target.value)} style={{ padding: "5px 8px" }}>
          <option value="">All statuses</option>
          <option value="DRAFT">DRAFT</option>
          <option value="ACTIVE">ACTIVE</option>
          <option value="PAUSED">PAUSED</option>
          <option value="SOLD">SOLD</option>
          <option value="UNAVAILABLE">UNAVAILABLE</option>
          <option value="EXPIRED">EXPIRED</option>
        </select>
      </div>

      {error && <p style={{ color: "crimson" }}>{error}</p>}

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "2px solid #ddd", fontSize: "0.85em" }}>
            <th style={{ padding: "8px 4px" }}>Status</th>
            <th style={{ padding: "8px 4px" }}>Product</th>
            <th style={{ padding: "8px 4px" }}>Variant</th>
            <th style={{ padding: "8px 4px" }}>Price</th>
            <th style={{ padding: "8px 4px" }}>Location</th>
            <th style={{ padding: "8px 4px" }}>Confidence</th>
            <th style={{ padding: "8px 4px" }}>Expires</th>
            <th style={{ padding: "8px 4px" }}></th>
          </tr>
        </thead>
        <tbody>
          {listings.map((l) => (
            <tr key={l.id} style={{ borderBottom: "1px solid #eee", fontSize: "0.9em" }}>
              <td style={{ padding: "8px 4px" }}>
                <span style={{ color: STATUS_COLORS[l.status], fontWeight: 600, fontSize: "0.85em" }}>{l.status}</span>
              </td>
              <td style={{ padding: "8px 4px" }}>{l.product ?? "—"}</td>
              <td style={{ padding: "8px 4px" }}>{l.variant ?? "—"}</td>
              <td style={{ padding: "8px 4px" }}>{l.price ? `₦${l.price.toLocaleString()}` : "—"}</td>
              <td style={{ padding: "8px 4px" }}>{l.location ?? "—"}</td>
              <td style={{ padding: "8px 4px" }}>{(l.confidence * 100).toFixed(0)}%</td>
              <td style={{ padding: "8px 4px", color: "#666", fontSize: "0.85em" }}>{new Date(l.expires_at).toLocaleDateString()}</td>
              <td style={{ padding: "8px 4px" }}>
                {l.status === "DRAFT" && (
                  <button onClick={() => handleActivate(l.id)} style={{ padding: "4px 10px", fontSize: "0.85em", cursor: "pointer" }}>
                    Activate
                  </button>
                )}
              </td>
            </tr>
          ))}
          {listings.length === 0 && (
            <tr><td colSpan={8} style={{ color: "#666", padding: 16 }}>No listings yet.</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
