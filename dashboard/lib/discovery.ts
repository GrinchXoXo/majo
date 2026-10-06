// Phase 4B Discovery & Distribution client (operator-only endpoints).
import { operatorCall } from "@/lib/api";

export type Reason = { dimension: string; points: number; detail: string };

export type EligibilitySnapshot = {
  eligible: boolean;
  lifecycle_day: number;
  lifecycle_max_days: number;
  expires_at: string;
  freshness: string;
  verified_at: string | null;
  geo_stage: string;
  geo_reason: string;
  viewer_level: string;
  eligible_for_viewer: boolean;
  viewer_reason: string;
  notes: string[];
};

export type PlannedCandidate = {
  listing_id: string;
  public_reference: string;
  product: string;
  category: string;
  price: number | null;
  score: number;
  reasons: Reason[];
  eligibility: EligibilitySnapshot;
  images: number;
  has_video: boolean;
  needs_reconfirmation: boolean;
};

export type Exclusion = { listing_id: string; public_reference: string; stage: string; code: string; detail?: string };

export type BuyerPlan = {
  buyer_id: string;
  recipient: string;
  planned: boolean;
  exclusion?: string;
  arm?: string;
  viewer_location?: string;
  candidates: PlannedCandidate[];
  listing_exclusions: Record<string, number>;
  deduped?: string[];
  excluded: Exclusion[];
};

export type PlanReport = {
  mode: string;
  send_allowed: boolean;
  ranking_version: string;
  presentation_version: string;
  arms: string[];
  evaluated_at: string;
  delivery_window_open: boolean;
  delivery_window?: string;
  supply_considered: number;
  eligible_listings: number;
  listing_exclusions: Record<string, number>;
  audience_considered: number;
  audience_eligible: number;
  deliveries: number;
  excluded_buyers: Record<string, number>;
  unknown_recipients: string[];
  buyers: BuyerPlan[];
};

export type Batch = {
  id: string;
  client_key: string;
  status: string;
  mode: string;
  ranking_version: string;
  presentation_version: string;
  arms: string[];
  plan: PlanReport;
  note?: string;
  created_at: string;
  sent_at: string | null;
};

export type Delivery = {
  id: string;
  batch_id: string;
  buyer_id: string;
  recipient: string;
  arm: string;
  status: string;
  consent_status: string;
  listing_ids: string[];
  candidates: PlannedCandidate[];
  presentation?: string;
  skip_reason?: string;
  error?: string;
  provider_message_id?: string;
  sent_at: string | null;
  interaction_count: number;
  last_action?: string;
};

export type Interaction = {
  id: string;
  raw_id: string;
  action?: string;
  listing_ref?: string;
  outcome: string;
  reason?: string;
  buyer_request_id: string | null;
  created_at: string;
};

export type Attribution = {
  messages: { message_id: string; provider_message_id: string; kind: string; sent_at: string; delivered_at: string | null; read_at: string | null }[];
  interactions: Interaction[];
  requests: { id: string; status: string; origin: string; listing_id: string | null }[];
  offers: { id: string; status: string; supply_listing_id: string | null; price: number | null }[];
  selections: { id: string; offer_id: string }[];
  transactions: { id: string; status: string; payment_status: string; gross_amount: number; supply_listing_id: string | null }[];
};

export type DeliveryDetail = {
  delivery: Delivery;
  permitted_now: { permitted: boolean; reasons: string[] };
  attribution: Attribution;
};

export type Funnel = Record<string, number> & { exclusions?: Record<string, number>; sent_by_arm?: Record<string, number> };

export type Settings = Record<string, unknown> & { mode: string; send_allowed: boolean; ranking_version: string };

export type Preference = {
  id: string;
  buyer_id: string;
  recipient: string;
  status: string;
  location: string | null;
  interests: string[];
  source: string;
  wording_version: string;
  captured_at: string;
  opted_out_at: string | null;
};

export type ScopeInput = { category?: string; query?: string; limit?: number };

export const getSettings = () => operatorCall<Settings>("/api/discovery/settings");
export const getPreferences = () => operatorCall<Preference[]>("/api/discovery/preferences");
export const setPreference = (body: { recipient: string; status?: string; location?: string; interests?: string[] }) =>
  operatorCall<Preference>("/api/discovery/preferences", "POST", body);
export const inviteConsent = (recipient: string) => operatorCall("/api/discovery/consent/invite", "POST", { recipient });
export const previewBatch = (scope: ScopeInput, recipients: string[]) =>
  operatorCall<PlanReport>("/api/discovery/preview", "POST", { scope, recipients });
export const createBatch = (client_key: string, scope: ScopeInput, recipients: string[], note: string) =>
  operatorCall<{ batch: Batch; created: boolean }>("/api/discovery/batches", "POST", { client_key, scope, recipients, note });
export const listBatches = () => operatorCall<Batch[]>("/api/discovery/batches");
export const getBatch = (id: string) => operatorCall<{ batch: Batch; deliveries: Delivery[] }>(`/api/discovery/batches/${id}`);
export const sendBatch = (id: string) =>
  operatorCall<{ status: string; sent: number; failed: number; skipped: Record<string, number>; already_sent: boolean }>(
    `/api/discovery/batches/${id}/send`,
    "POST",
  );
export const getDelivery = (id: string) => operatorCall<DeliveryDetail>(`/api/discovery/deliveries/${id}`);
export const getDiscoveryFunnel = (batchId?: string) =>
  operatorCall<Funnel>(`/api/discovery/funnel${batchId ? `?batch_id=${batchId}` : ""}`);

export function splitList(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);
}

// The discovery funnel in reading order (Lock §K.1).
export const FUNNEL_ROWS: [string, string][] = [
  ["opted_in", "Opted in"],
  ["eligible_audience", "Eligible audience"],
  ["eligible_listings", "Eligible listings"],
  ["deliveries_planned", "Deliveries planned"],
  ["deliveries_attempted", "Deliveries attempted"],
  ["deliveries_sent", "Sent"],
  ["delivered", "Delivered"],
  ["read", "Read"],
  ["interacted", "Interacted"],
  ["listing_viewed", "Listing viewed / video"],
  ["asked", "Ask about it"],
  ["requests_created", "BuyerRequests created"],
  ["requests_confirmed", "BuyerRequests confirmed"],
  ["offers", "Offers"],
  ["selections", "Selections"],
  ["transactions", "Transactions"],
  ["opt_outs_from_discovery", "Opt-outs (Stop)"],
  ["unresolved_interactions", "Unresolved interactions"],
];
