// Thin API client. The dashboard talks to the Go backend only — never
// directly to PostgreSQL (PRD 0 FINAL §24). This is the single place that
// knows the backend's base URL, so it can change without touching every page.

const API_URL = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";

export type HealthStatus = {
  status: string;
  checks?: Record<string, string>;
};

export async function getHealth(): Promise<HealthStatus> {
  const res = await fetch(`${API_URL}/health`, { cache: "no-store" });
  if (!res.ok) throw new Error(`health check failed: ${res.status}`);
  return res.json();
}

export type FunnelStage = { key: string; label: string; count: number; source: string; note?: string };
export type FunnelReport = { since: string; until: string; stages: FunnelStage[] };

// Core marketplace funnel (Phase 3.1 §23A), read-only, counted from the
// system-of-record tables.
export async function getFunnel(days = 30): Promise<FunnelReport> {
  const res = await fetch(`${API_URL}/api/metrics/funnel?days=${days}`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load funnel: ${res.status}`);
  return res.json();
}

export async function getReadiness(): Promise<HealthStatus> {
  const res = await fetch(`${API_URL}/ready`, { cache: "no-store" });
  return res.json(); // may be 200 or 503; both are valid, informative responses
}

// --- BuyerRequest types & client (Phase 1) ---

export type BuyerRequestDTO = {
  id: string;
  conversation_id: string;
  buyer_id: string;
  status: "EXTRACTING" | "NEEDS_REVIEW" | "CONFIRMED";
  category: string | null;
  product: string | null;
  variant: string | null;
  budget_min: number | null;
  budget_max: number | null;
  currency: string;
  location: string | null;
  condition: string | null;
  quantity: number | null;
  urgency: string | null;
  preferences: string[];
  raw_message: string;
  extracted_data: Record<string, unknown>;
  confidence: number;
  completeness: "complete" | "partially_complete" | "insufficient";
  created_at: string;
  updated_at: string;
  confirmed_at: string | null;
};

export type RequestEventDTO = {
  event_type: string;
  actor_type: string;
  payload: Record<string, unknown>;
  created_at: string;
};

export async function listRequests(status?: string): Promise<BuyerRequestDTO[]> {
  const url = status
    ? `${API_URL}/api/requests?status=${encodeURIComponent(status)}`
    : `${API_URL}/api/requests`;
  const res = await fetch(url, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to list requests: ${res.status}`);
  const data = await res.json();
  return data ?? [];
}

export async function getRequest(id: string): Promise<BuyerRequestDTO> {
  const res = await fetch(`${API_URL}/api/requests/${id}`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load request: ${res.status}`);
  return res.json();
}

export async function getRequestEvents(id: string): Promise<RequestEventDTO[]> {
  const res = await fetch(`${API_URL}/api/requests/${id}/events`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load events: ${res.status}`);
  const data = await res.json();
  return data ?? [];
}

export type PatchRequestBody = Partial<{
  category: string;
  product: string;
  variant: string;
  budget_min: number;
  budget_max: number;
  location: string;
  condition: string;
  quantity: number;
  urgency: string;
  preferences: string[];
}>;

export async function patchRequest(id: string, body: PatchRequestBody): Promise<BuyerRequestDTO> {
  const res = await fetch(`${API_URL}/api/requests/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`failed to update request: ${res.status}`);
  return res.json();
}

export async function confirmRequest(id: string): Promise<BuyerRequestDTO> {
  const res = await fetch(`${API_URL}/api/requests/${id}/confirm`, { method: "POST" });
  if (!res.ok) throw new Error(`failed to confirm request: ${res.status}`);
  return res.json();
}

export async function clarifyRequest(id: string, question?: string): Promise<void> {
  const res = await fetch(`${API_URL}/api/requests/${id}/clarify`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ question: question ?? "" }),
  });
  if (!res.ok) throw new Error(`failed to send clarification: ${res.status}`);
}

// --- Vendor & matching types & client (Phase 2) ---

export type VendorDTO = {
  id: string;
  business_name: string;
  whatsapp_external_id: string;
  phone: string;
  status: "PENDING" | "ACTIVE" | "INACTIVE" | "BLOCKED";
  primary_location: string;
  notes: string;
  categories: string[];
  service_areas: string[];
  requests_received: number;
  responses_received: number;
  created_at: string;
};

export type MatchCandidateDTO = {
  vendor_id: string;
  vendor_name: string;
  score: number;
  match_type: string;
  breakdown: Record<string, number>;
  reasons: string[];
  within_budget: boolean | null;
  relevant_supply?: Record<string, unknown>;
};

export type VendorResponseDTO = {
  id: string;
  vendor_request_id: string | null;
  vendor_id: string;
  status: "AVAILABLE" | "UNAVAILABLE" | "CAN_SOURCE" | "ALTERNATIVE" | "UNCLASSIFIED";
  review_state: "NEEDS_REVIEW" | "REVIEWED" | "CORRECTED";
  product: string | null;
  variant: string | null;
  condition: string | null;
  price: number | null;
  currency: string;
  quantity: number | null;
  location: string | null;
  notes: string | null;
  raw_message: string;
  confidence: number;
  extraction_method: "heuristic" | "ai" | "hybrid" | "manual";
  created_at: string;
};

export type VendorRequestDTO = {
  id: string;
  vendor_id: string;
  public_reference: string;
  status: "PENDING" | "SENT" | "SEND_FAILED" | "RESPONDED" | "EXPIRED" | "CANCELLED";
  match_score: number | null;
  created_at: string;
  sent_at: string | null;
  send_error: string | null;
  send_failed_at: string | null;
  response?: VendorResponseDTO;
};

export async function listVendors(status?: string): Promise<VendorDTO[]> {
  const url = status ? `${API_URL}/api/vendors?status=${encodeURIComponent(status)}` : `${API_URL}/api/vendors`;
  const res = await fetch(url, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to list vendors: ${res.status}`);
  return (await res.json()) ?? [];
}

export async function getVendor(id: string): Promise<VendorDTO> {
  const res = await fetch(`${API_URL}/api/vendors/${id}`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load vendor: ${res.status}`);
  return res.json();
}

export type CreateVendorBody = {
  business_name: string;
  whatsapp_external_id: string;
  phone?: string;
  primary_location?: string;
  notes?: string;
  categories: string[];
  service_areas: string[];
};

export async function createVendor(body: CreateVendorBody): Promise<VendorDTO> {
  const res = await fetch(`${API_URL}/api/vendors`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`failed to create vendor: ${res.status}`);
  return res.json();
}

export async function patchVendor(id: string, body: Partial<CreateVendorBody & { status: string }>): Promise<VendorDTO> {
  const res = await fetch(`${API_URL}/api/vendors/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`failed to update vendor: ${res.status}`);
  return res.json();
}

export async function getVendorSupply(id: string): Promise<Record<string, unknown>[]> {
  const res = await fetch(`${API_URL}/api/vendors/${id}/supply`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load supply: ${res.status}`);
  return (await res.json()) ?? [];
}

export async function getMatches(requestId: string): Promise<MatchCandidateDTO[]> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/matches`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load matches: ${res.status}`);
  return (await res.json()) ?? [];
}

export async function routeToVendors(requestId: string, vendorIds: string[]): Promise<VendorRequestDTO[]> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/route`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ vendor_ids: vendorIds }),
  });
  if (!res.ok) throw new Error(`failed to route to vendors: ${res.status}`);
  return res.json();
}

export async function getVendorRequests(requestId: string): Promise<VendorRequestDTO[]> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/vendor-requests`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load vendor requests: ${res.status}`);
  return (await res.json()) ?? [];
}

// --- Vendor response correction & association (Phase 2.1) ---

export async function getUnmatchedResponses(): Promise<VendorResponseDTO[]> {
  const res = await fetch(`${API_URL}/api/vendor-responses/unmatched`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load unmatched responses: ${res.status}`);
  return (await res.json()) ?? [];
}

export async function getVendorResponse(id: string): Promise<VendorResponseDTO> {
  const res = await fetch(`${API_URL}/api/vendor-responses/${id}`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load vendor response: ${res.status}`);
  return res.json();
}

export type PatchVendorResponseBody = Partial<{
  status: string;
  product: string;
  variant: string;
  condition: string;
  price: number;
  quantity: number;
  location: string;
  notes: string;
}>;

export async function patchVendorResponse(id: string, body: PatchVendorResponseBody): Promise<VendorResponseDTO> {
  const res = await fetch(`${API_URL}/api/vendor-responses/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`failed to update vendor response: ${res.status} ${text}`);
  }
  return res.json();
}

export async function associateVendorResponse(id: string, vendorRequestId: string): Promise<VendorResponseDTO> {
  const res = await fetch(`${API_URL}/api/vendor-responses/${id}/associate`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ vendor_request_id: vendorRequestId }),
  });
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`failed to associate vendor response: ${res.status} ${text}`);
  }
  return res.json();
}

export async function getVendorOwnRequests(vendorId: string): Promise<VendorRequestDTO[]> {
  const res = await fetch(`${API_URL}/api/vendors/${vendorId}/vendor-requests`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load vendor's requests: ${res.status}`);
  return (await res.json()) ?? [];
}

// --- Phase 3: Supply listings, offers, transactions ---

export type ListingStatus = "DRAFT" | "ACTIVE" | "PAUSED" | "SOLD" | "UNAVAILABLE" | "EXPIRED" | "ARCHIVED";

export type ListingMediaRefDTO = {
  type: string;
  asset_id?: string;
  role?: string; // "PRIMARY" on the operator-chosen primary image
};

export type SupplyListingDTO = {
  id: string;
  vendor_id: string;
  supply_signal_id: string | null;
  public_reference: string;
  category: string | null;
  product: string | null;
  variant: string | null;
  condition: string | null;
  price: number | null;
  currency: string;
  quantity: number | null;
  location: string | null;
  description: string | null;
  media_references: ListingMediaRefDTO[] | null;
  status: ListingStatus;
  extraction_method: string;
  confidence: number;
  published_at: string | null;
  last_verified_at: string | null;
  expires_at: string;
  created_at: string;
  updated_at: string; // full precision; send back as expected_updated_at
  rejected_at: string | null;
  rejection_reason: string | null;
  primary_media_asset_id: string | null;
};

export async function searchSupplyListings(params?: Record<string, string>): Promise<SupplyListingDTO[]> {
  const query = params ? "?" + new URLSearchParams(params).toString() : "";
  const res = await fetch(`${API_URL}/api/supply-listings${query}`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to search listings: ${res.status}`);
  return (await res.json()) ?? [];
}

// --- Phase 4A Step 4: listing curation (operator-only actions) ---

// The operator token is entered once per browser session and kept in
// sessionStorage; it is never part of the build (NEXT_PUBLIC_* would ship
// it to every visitor).
const TOKEN_KEY = "majo.operatorToken";

export function getOperatorToken(): string {
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setOperatorToken(token: string): void {
  try {
    if (token) sessionStorage.setItem(TOKEN_KEY, token);
    else sessionStorage.removeItem(TOKEN_KEY);
  } catch {
    // storage unavailable: the token lives only for this page view
  }
}

function operatorHeaders(json = true): HeadersInit {
  const h: Record<string, string> = {};
  if (json) h["Content-Type"] = "application/json";
  const t = getOperatorToken();
  if (t) h["Authorization"] = `Bearer ${t}`;
  return h;
}

export type FieldProblem = { field: string; message: string };

// ApiError carries the backend's stable error code (UNAUTHORIZED, STALE,
// VALIDATION, NOT_PUBLISHABLE, INVALID_TRANSITION, NOT_FOUND, ...).
export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string, public problems: FieldProblem[] = []) {
    super(message);
  }
}

async function curationCall<T>(path: string, init: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`${API_URL}${path}`, { cache: "no-store", ...init });
  } catch {
    throw new ApiError(0, "NETWORK", "Could not reach the backend.");
  }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new ApiError(res.status, body.code ?? "ERROR", body.error ?? `request failed: ${res.status}`, body.problems ?? []);
  }
  return body as T;
}

export type ListingDetailDTO = SupplyListingDTO & {
  vendor: { id: string; business_name: string; status: string; primary_location: string } | null;
  source_signal: {
    id: string;
    category: string | null;
    product: string | null;
    variant: string | null;
    condition: string | null;
    price: number | null;
    currency: string;
    quantity: number | null;
    location: string | null;
    availability_status: string;
    raw_message: string;
    confidence: number;
    created_at: string;
    expires_at: string;
  } | null;
  source_messages: {
    position: number;
    message_id: string;
    external_message_id: string | null;
    message_type: string;
    text: string | null;
    media_asset_id: string | null;
    received_at: string;
  }[];
  media: {
    order: number;
    asset_id: string;
    type: string;
    role?: string;
    primary: boolean;
    status: string; // AVAILABLE | REMOVED | MISSING
    available: boolean;
    mime_type?: string;
    size_bytes?: number;
    preview_path?: string;
  }[];
  published: boolean;
  rejected: boolean;
  publish_blockers: FieldProblem[];
};

export async function getListingDetail(id: string): Promise<ListingDetailDTO> {
  return curationCall<ListingDetailDTO>(`/api/supply-listings/${id}`, { method: "GET" });
}

export type ListingEditBody = {
  category?: string;
  product?: string;
  variant?: string;
  condition?: string;
  price?: number;
  currency?: string;
  quantity?: number;
  location?: string;
  description?: string;
  media_order?: string[];
  primary_media_asset_id?: string;
  expected_updated_at?: string;
};

export async function patchListing(id: string, body: ListingEditBody): Promise<SupplyListingDTO> {
  return curationCall(`/api/supply-listings/${id}`, { method: "PATCH", headers: operatorHeaders(), body: JSON.stringify(body) });
}

export async function publishListing(id: string, expectedUpdatedAt?: string): Promise<SupplyListingDTO> {
  return curationCall(`/api/supply-listings/${id}/publish`, {
    method: "POST",
    headers: operatorHeaders(),
    body: JSON.stringify(expectedUpdatedAt ? { expected_updated_at: expectedUpdatedAt } : {}),
  });
}

export async function rejectListing(id: string, reason: string, expectedUpdatedAt?: string): Promise<SupplyListingDTO> {
  const body: Record<string, string> = {};
  if (reason.trim()) body.reason = reason.trim();
  if (expectedUpdatedAt) body.expected_updated_at = expectedUpdatedAt;
  return curationCall(`/api/supply-listings/${id}/reject`, { method: "POST", headers: operatorHeaders(), body: JSON.stringify(body) });
}

// fetchListingMedia returns an object URL for one of a listing's media
// (revoke it when done), or throws ApiError (e.g. MEDIA_UNAVAILABLE).
export async function fetchListingMedia(listingId: string, assetId: string): Promise<string> {
  let res: Response;
  try {
    res = await fetch(`${API_URL}/api/supply-listings/${listingId}/media/${assetId}`, {
      cache: "no-store",
      headers: operatorHeaders(false),
    });
  } catch {
    throw new ApiError(0, "NETWORK", "Could not reach the backend.");
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new ApiError(res.status, body.code ?? "ERROR", body.error ?? "media unavailable");
  }
  return URL.createObjectURL(await res.blob());
}

export type OfferDTO = {
  id: string;
  buyer_request_id: string;
  vendor_id: string;
  vendor_response_id: string | null;
  supply_listing_id: string | null;
  type: "EXACT" | "ALTERNATIVE" | "CAN_SOURCE" | "PARTIAL";
  status: string;
  product: string | null;
  variant: string | null;
  condition: string | null;
  price: number | null;
  currency: string;
  ranking_score: number;
  ranking_reasons: string[];
  confidence: number;
  created_at: string;
  // Phase 3.1 delivery tracking
  presentation_ordinal: number | null;
  sent_at: string | null;
  expires_at: string | null;
  delivery_attempts: number;
  last_delivery_error: string | null;
};

// Read-only since Phase 3.1: offers are built and delivered automatically
// when vendor replies arrive; viewing never builds or sends anything.
export async function getOffers(requestId: string): Promise<OfferDTO[]> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/offers`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load offers: ${res.status}`);
  return (await res.json()) ?? [];
}

export type DeliveryResultDTO = {
  delivered: number;
  ordinals: number[] | null;
  highest_ordinal: number;
  skipped?: string;
};

// Explicit operator action: build newly eligible offers and deliver any
// not-yet-delivered ones (e.g. retry after a failed send). Idempotent —
// already-delivered offers are never re-sent.
export async function deliverOffers(requestId: string): Promise<DeliveryResultDTO> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/offers/deliver`, { method: "POST" });
  if (!res.ok) throw new Error(await errorText(res, "failed to deliver offers"));
  return res.json();
}

export type SelectionDTO = {
  selection: { id: string; offer_id: string; buyer_id: string; status: string; created_at: string } | null;
  transaction: {
    id: string;
    status: string;
    payment_status: string;
    fulfilment_status: string;
    gross_amount: number;
    platform_fee: number;
    vendor_amount: number;
    currency: string;
    created_at: string;
  } | null;
};

export async function getSelection(requestId: string): Promise<SelectionDTO> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/selection`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load selection: ${res.status}`);
  return res.json();
}

async function errorText(res: Response, fallback: string): Promise<string> {
  try {
    const body = await res.json();
    return body?.error ? `${fallback}: ${body.error}` : `${fallback}: ${res.status}`;
  } catch {
    return `${fallback}: ${res.status}`;
  }
}

export async function getOfferPreview(requestId: string): Promise<string> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/offers/preview`, { cache: "no-store" });
  if (!res.ok) throw new Error(`failed to load offer preview: ${res.status}`);
  const data = await res.json();
  return data.message;
}

export async function selectOffer(requestId: string, offerId: string, buyerId: string): Promise<{ transaction_id: string; payment_status: string; gross_amount: number }> {
  const res = await fetch(`${API_URL}/api/requests/${requestId}/select`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ offer_id: offerId, buyer_id: buyerId }),
  });
  if (!res.ok) throw new Error(await errorText(res, "failed to select offer"));
  return res.json();
}
