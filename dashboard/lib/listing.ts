// Small display helpers shared by the listing curation pages.

export const STATUS_COLORS: Record<string, string> = {
  DRAFT: "#6b7280",
  ACTIVE: "#15803d",
  PAUSED: "#b45309",
  SOLD: "#7c3aed",
  UNAVAILABLE: "#dc2626",
  EXPIRED: "#9ca3af",
  ARCHIVED: "#9ca3af",
};

export function formatPrice(price: number | null | undefined, currency: string): string {
  if (price == null) return "—";
  return currency === "NGN" ? `₦${price.toLocaleString()}` : `${currency} ${price.toLocaleString()}`;
}

export function formatDate(s: string | null | undefined): string {
  return s ? new Date(s).toLocaleString() : "—";
}

// Phase 4A.5 lifecycle helpers.
const GEO_LABELS: Record<string, string> = {
  NONE: "Not searchable",
  PENDING: "Not yet searchable",
  LOCAL: "Origin area",
  NEARBY: "Nearby areas",
  STATE: "Whole state",
  REGION: "Broader region",
  EXPIRED: "Expired",
};

export function geoLabel(stage: string | undefined): string {
  return stage ? GEO_LABELS[stage] ?? stage : "—";
}

export function relativeDays(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return "—";
  const ms = new Date(iso).getTime() - now;
  const d = Math.round(Math.abs(ms) / 86_400_000);
  const h = Math.round(Math.abs(ms) / 3_600_000);
  const amount = d >= 1 ? `${d} day${d === 1 ? "" : "s"}` : `${h} hour${h === 1 ? "" : "s"}`;
  return ms >= 0 ? `in ${amount}` : `${amount} ago`;
}

export const FRESHNESS_COLORS: Record<string, string> = { FRESH: "#15803d", STALE: "#b45309", UNVERIFIED: "#6b7280" };
