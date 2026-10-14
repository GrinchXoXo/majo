"use client";

import { useEffect, useState, useCallback } from "react";
import { ApiError, searchSupplyListings, listVendors, type SupplyListingDTO } from "@/lib/api";
import ListingMedia from "@/components/ListingMedia";
import { FRESHNESS_COLORS, STATUS_COLORS, formatDate, formatPrice, geoLabel } from "@/lib/listing";

// Supply listing curation queue (Phase 4A Step 4). By default it shows the
// work: DRAFT listings, newest first. Other statuses are separate filters,
// so published or rejected listings never look like new drafts.

function thumbOf(l: SupplyListingDTO) {
  const refs = l.media_references ?? [];
  const primary = refs.find((m) => m.asset_id && m.asset_id === l.primary_media_asset_id);
  return primary ?? refs.find((m) => m.type === "image" && m.asset_id) ?? null;
}

// Each filter is a status plus a lifecycle scope (Phase 4A.5): by default
// only listings inside their lifecycle; "expired" shows past ones (history).
const FILTERS: { value: string; label: string; params: Record<string, string> }[] = [
  { value: "DRAFT", label: "Needs review (DRAFT)", params: { status: "DRAFT" } },
  { value: "ACTIVE", label: "Published (ACTIVE, live)", params: { status: "ACTIVE" } },
  { value: "ACTIVE_EXPIRED", label: "Published but past lifecycle (history)", params: { status: "ACTIVE", lifecycle: "expired" } },
  { value: "ARCHIVED", label: "Rejected / archived", params: { status: "ARCHIVED", lifecycle: "all" } },
  { value: "PAUSED", label: "PAUSED", params: { status: "PAUSED" } },
  { value: "SOLD", label: "SOLD", params: { status: "SOLD", lifecycle: "all" } },
  { value: "UNAVAILABLE", label: "UNAVAILABLE", params: { status: "UNAVAILABLE", lifecycle: "all" } },
];

export default function SupplyListingsPage() {
  const [listings, setListings] = useState<SupplyListingDTO[] | null>(null);
  const [vendors, setVendors] = useState<Record<string, string>>({});
  const [statusFilter, setStatusFilter] = useState("DRAFT");
  const [error, setError] = useState<string | null>(null);
  const [tokenTick, setTokenTick] = useState(0);

  const load = useCallback(async () => {
    try {
      setListings(await searchSupplyListings(FILTERS.find((f) => f.value === statusFilter)?.params ?? { status: statusFilter }));
      setError(null);
    } catch (e) {
      setListings(null);
      setError(
        e instanceof ApiError && e.code === "UNAUTHORIZED"
          ? "Enter the operator token (top right) to see listings."
          : e instanceof Error
            ? e.message
            : "Failed to load listings",
      );
    }
  }, [statusFilter, tokenTick]);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => {
    listVendors()
      .then((vs) => setVendors(Object.fromEntries(vs.map((v) => [v.id, v.business_name]))))
      .catch(() => setVendors({}));
  }, []);

  const th: React.CSSProperties = { padding: "8px 4px" };
  const td: React.CSSProperties = { padding: "6px 4px", verticalAlign: "middle" };

  return (
    <div>
      <title>Supply Listings · Majo</title>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", flexWrap: "wrap", gap: 8 }}>
        <h1 style={{ margin: "8px 0" }}>Supply Listings</h1>
      </div>
      <p style={{ color: "#666", marginTop: 0 }}>
        Listings are created as DRAFT from vendor supply. Open one to check it, correct it, and publish or reject it. Nothing
        is shown to buyers until an operator publishes it.
      </p>

      <div style={{ marginBottom: 12 }}>
        <label>
          Show{" "}
          <select
            aria-label="Status filter"
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
            style={{ padding: "5px 8px" }}
          >
            {FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </label>{" "}
        <button onClick={load}>Refresh</button>
      </div>

      {error && <p style={{ color: "crimson" }}>{error}</p>}

      <div style={{ overflowX: "auto" }}>
        <table style={{ width: "100%", borderCollapse: "collapse" }} data-testid="listing-queue">
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "2px solid #ddd", fontSize: "0.85em" }}>
              <th style={th}></th>
              <th style={th}>Ref</th>
              <th style={th}>Product</th>
              <th style={th}>Category</th>
              <th style={th}>Price</th>
              <th style={th}>Vendor</th>
              <th style={th}>Location</th>
              <th style={th}>Condition</th>
              <th style={th}>Media</th>
              <th style={th}>Created</th>
              <th style={th}>Verified</th>
              <th style={th}>Freshness</th>
              <th style={th}>Lifecycle</th>
              <th style={th}>Status</th>
            </tr>
          </thead>
          <tbody>
            {(listings ?? []).map((l) => {
              const thumb = thumbOf(l);
              const mediaCount = (l.media_references ?? []).length;
              return (
                <tr key={l.id} style={{ borderBottom: "1px solid #eee", fontSize: "0.9em" }} data-testid="listing-row">
                  <td style={td}>
                    {thumb?.asset_id ? (
                      <a href={`/offers/${l.id}`}>
                        <ListingMedia listingId={l.id} assetId={thumb.asset_id} type={thumb.type} size={48} reloadKey={tokenTick} />
                      </a>
                    ) : (
                      <div style={{ width: 48, height: 48, background: "#f9fafb", border: "1px dashed #d1d5db", fontSize: "0.65em", color: "#9ca3af", display: "flex", alignItems: "center", justifyContent: "center" }}>
                        No image
                      </div>
                    )}
                  </td>
                  <td style={td}>
                    <a href={`/offers/${l.id}`} style={{ fontFamily: "monospace" }}>
                      {l.public_reference}
                    </a>
                  </td>
                  <td style={td}>
                    <a href={`/offers/${l.id}`}>{l.product ?? "—"}</a>
                    {l.variant ? <span style={{ color: "#666" }}> · {l.variant}</span> : null}
                  </td>
                  <td style={td}>{l.category ?? "—"}</td>
                  <td style={td}>{formatPrice(l.price, l.currency)}</td>
                  <td style={td}>{vendors[l.vendor_id] ?? l.vendor_id.slice(0, 8)}</td>
                  <td style={td}>{l.location ?? "—"}</td>
                  <td style={td}>{l.condition ?? "—"}</td>
                  <td style={td}>{mediaCount === 0 ? "none" : mediaCount}</td>
                  <td style={{ ...td, color: "#666", fontSize: "0.85em" }}>{formatDate(l.created_at)}</td>
                  <td style={{ ...td, color: "#666", fontSize: "0.85em" }}>{formatDate(l.last_verified_at)}</td>
                  <td style={td} data-testid="row-freshness">
                    {l.eligibility ? (
                      <span style={{ color: FRESHNESS_COLORS[l.eligibility.freshness.state], fontWeight: 600, fontSize: "0.85em" }}>
                        {l.eligibility.freshness.state}
                      </span>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td style={{ ...td, fontSize: "0.85em" }} data-testid="row-lifecycle">
                    {!l.eligibility
                      ? "—"
                      : l.eligibility.lifecycle.state === "LIVE"
                        ? `Day ${l.eligibility.lifecycle.day} / ${l.eligibility.lifecycle.max_days} · ${geoLabel(l.eligibility.geography.stage)}`
                        : l.eligibility.lifecycle.state === "EXPIRED"
                          ? "Expired"
                          : l.eligibility.lifecycle.state === "NOT_PUBLISHED"
                            ? "Not started"
                            : "Closed"}
                  </td>
                  <td style={td}>
                    <span style={{ color: STATUS_COLORS[l.status], fontWeight: 600, fontSize: "0.85em" }}>
                      {l.status === "ARCHIVED" && l.rejected_at ? "REJECTED" : l.status}
                    </span>
                  </td>
                </tr>
              );
            })}
            {listings !== null && listings.length === 0 && (
              <tr>
                <td colSpan={14} style={{ color: "#666", padding: 16 }} data-testid="queue-empty">
                  {statusFilter === "DRAFT" ? "No drafts waiting for review." : "No listings with this status."}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
