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
