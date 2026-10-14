"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "next/navigation";
import {
  ApiError,
  getListingDetail,
  patchListing,
  publishListing,
  rejectListing,
  verifyListing,
  type FieldProblem,
  type ListingDetailDTO,
  type ListingEditBody,
} from "@/lib/api";
import ListingMedia from "@/components/ListingMedia";
import { FRESHNESS_COLORS, STATUS_COLORS, formatDate, formatPrice, geoLabel, relativeDays } from "@/lib/listing";

// Listing curation (Phase 4A Step 4): inspect a listing next to the
// evidence it came from, correct marketplace fields, arrange media and
// choose the primary image, then publish or reject. Every change is a
// server call; the page only reports success after the server confirms.

type Form = {
  product: string;
  category: string;
  variant: string;
  condition: string;
  price: string;
  currency: string;
  quantity: string;
  location: string;
  description: string;
};

const TEXT_FIELDS = ["product", "category", "variant", "condition", "location", "description"] as const;

function toForm(l: ListingDetailDTO): Form {
  return {
    product: l.product ?? "",
    category: l.category ?? "",
    variant: l.variant ?? "",
    condition: l.condition ?? "",
    price: l.price?.toString() ?? "",
    currency: l.currency ?? "NGN",
    quantity: l.quantity?.toString() ?? "",
    location: l.location ?? "",
    description: l.description ?? "",
  };
}

type Notice = { kind: "ok" | "error"; text: string; problems?: FieldProblem[] } | null;

function describeError(e: unknown): Notice {
  if (e instanceof ApiError) {
    const hint: Record<string, string> = {
      UNAUTHORIZED: "Operator authorization required: enter the operator token at the top of the page.",
      STALE: "This listing was changed in another tab or by another operator. Reload to see the latest version.",
      INVALID_TRANSITION: e.message,
      NOT_FOUND: "Listing not found.",
      NETWORK: e.message,
    };
    return { kind: "error", text: hint[e.code] ?? e.message, problems: e.problems };
  }
  return { kind: "error", text: e instanceof Error ? e.message : "Something went wrong." };
}

export default function ListingCurationPage() {
  const params = useParams();
  const id = params.id as string;

  const [detail, setDetail] = useState<ListingDetailDTO | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [form, setForm] = useState<Form | null>(null);
  const [order, setOrder] = useState<string[]>([]);
  const [primary, setPrimary] = useState<string | null>(null);
  const [notice, setNotice] = useState<Notice>(null);
  const [busy, setBusy] = useState(false);
  const [reason, setReason] = useState("");
  const [verifyNote, setVerifyNote] = useState("");
  const [tokenTick, setTokenTick] = useState(0);

  const reset = (d: ListingDetailDTO) => {
    setDetail(d);
    setForm(toForm(d));
    setOrder(d.media.map((m) => m.asset_id));
    setPrimary(d.media.find((m) => m.primary)?.asset_id ?? null);
  };

  const load = useCallback(async () => {
    try {
      reset(await getListingDetail(id));
      setLoadError(null);
    } catch (e) {
      setLoadError(e instanceof ApiError && e.code === "NOT_FOUND" ? "Listing not found." : describeError(e)?.text ?? "Failed to load");
    }
  }, [id]);

  useEffect(() => {
    load();
  }, [load]);

  // Re-read after an action. Never turns a completed action into an error:
  // if the reload fails the action's own result still stands.
  const refresh = async (): Promise<boolean> => {
    try {
      reset(await getListingDetail(id));
      return true;
    } catch {
      return false;
    }
  };
  const staleNote = (fresh: boolean) => (fresh ? "" : " (The page could not be reloaded; refresh it to see the latest state.)");

  // The edit body: only fields that actually changed.
  const changes = useMemo<ListingEditBody>(() => {
    if (!detail || !form) return {};
    const out: ListingEditBody = {};
    const base = toForm(detail);
    for (const f of TEXT_FIELDS) {
      if (form[f] !== base[f]) out[f] = form[f];
    }
    if (form.price !== base.price) out.price = form.price.trim() === "" ? NaN : Number(form.price);
    if (form.quantity !== base.quantity) out.quantity = form.quantity.trim() === "" ? NaN : Number(form.quantity);
    if (form.currency !== base.currency) out.currency = form.currency;
    const baseOrder = detail.media.map((m) => m.asset_id);
    if (order.join() !== baseOrder.join()) out.media_order = order;
    const basePrimary = detail.media.find((m) => m.primary)?.asset_id ?? null;
    if (primary && primary !== basePrimary) out.primary_media_asset_id = primary;
    return out;
  }, [detail, form, order, primary]);
  const dirty = Object.keys(changes).length > 0;

  if (loadError) {
    return (
      <div>
        <p>
          <a href="/offers">← Supply listings</a>
        </p>
        <p style={{ color: "crimson" }} data-testid="load-error">
          {loadError}
        </p>
      </div>
    );
  }
  if (!detail || !form) return <p>Loading…</p>;

  const editable = detail.status === "DRAFT" || detail.status === "ACTIVE";
  const mediaById = Object.fromEntries(detail.media.map((m) => [m.asset_id, m]));
  const sig = detail.source_signal;

  const save = async () => {
    for (const k of ["price", "quantity"] as const) {
      const v = changes[k];
      if (v !== undefined && (!Number.isFinite(v) || !Number.isInteger(v))) {
        setNotice({ kind: "error", text: `${k} must be a whole number (it cannot be cleared here).` });
        return;
      }
    }
    setBusy(true);
    setNotice(null);
    try {
      await patchListing(id, { ...changes, expected_updated_at: detail.updated_at });
      setNotice({ kind: "ok", text: "Changes saved." + staleNote(await refresh()) });
    } catch (e) {
      setNotice(describeError(e));
    } finally {
      setBusy(false);
    }
  };

  const publish = async () => {
    setBusy(true);
    setNotice(null);
    try {
      const l = await publishListing(id, detail.updated_at);
      const fresh = await refresh();
      setNotice(
        l.status === "ACTIVE"
          ? { kind: "ok", text: `Published ${l.public_reference}.` + staleNote(fresh) }
          : { kind: "error", text: `Unexpected status ${l.status}.` },
      );
    } catch (e) {
      setNotice(describeError(e));
      await refresh();
    } finally {
      setBusy(false);
    }
  };

  const reject = async () => {
    setBusy(true);
    setNotice(null);
    try {
      const l = await rejectListing(id, reason, detail.updated_at);
      const fresh = await refresh();
      setReason("");
      setNotice(
        l.status === "ARCHIVED" ? { kind: "ok", text: "Listing rejected." + staleNote(fresh) } : { kind: "error", text: `Unexpected status ${l.status}.` },
      );
    } catch (e) {
      setNotice(describeError(e));
    } finally {
      setBusy(false);
    }
  };

  const verify = async () => {
    setBusy(true);
    setNotice(null);
    try {
      await verifyListing(id, verifyNote, detail.updated_at);
      const fresh = await refresh();
      setVerifyNote("");
      setNotice({ kind: "ok", text: "Marked as verified: availability confirmed now." + staleNote(fresh) });
    } catch (e) {
      setNotice(describeError(e));
    } finally {
      setBusy(false);
    }
  };

  const move = (i: number, d: number) => {
    const j = i + d;
    if (j < 0 || j >= order.length) return;
    const next = [...order];
    [next[i], next[j]] = [next[j], next[i]];
    setOrder(next);
  };

  const field = (name: keyof Form, label: string, opts: { type?: string; evidence?: string | number | null } = {}) => (
    <tr key={name}>
      <td style={{ padding: "4px 8px 4px 0", color: "#555", whiteSpace: "nowrap" }}>
        <label htmlFor={`f-${name}`}>{label}</label>
      </td>
      <td style={{ padding: "4px 0" }}>
        {name === "currency" ? (
          <select id={`f-${name}`} value={form.currency} disabled={!editable} onChange={(e) => setForm({ ...form, currency: e.target.value })}>
            <option value="NGN">NGN</option>
          </select>
        ) : (
          <input
            id={`f-${name}`}
            name={name}
            type={opts.type ?? "text"}
            value={form[name]}
            disabled={!editable}
            onChange={(e) => setForm({ ...form, [name]: e.target.value })}
            style={{ padding: "4px 6px", width: "100%", maxWidth: 320, boxSizing: "border-box" }}
          />
        )}
      </td>
      <td style={{ padding: "4px 8px", color: "#6b7280", fontSize: "0.85em" }}>{opts.evidence ?? "—"}</td>
    </tr>
  );

  const card: React.CSSProperties = { border: "1px solid #e5e7eb", borderRadius: 6, padding: 12, marginBottom: 16 };
  const statusLabel = detail.status === "ARCHIVED" && detail.rejected ? "REJECTED (ARCHIVED)" : detail.status;

  return (
    <div style={{ maxWidth: 1100 }}>
      <title>Supply listing · Majo</title>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", flexWrap: "wrap", gap: 8 }}>
        <a href="/offers">← Supply listings</a>
      </div>

      <h1 style={{ marginBottom: 4 }}>
        <span style={{ fontFamily: "monospace" }} data-testid="public-ref">
          {detail.public_reference}
        </span>{" "}
        <span style={{ color: STATUS_COLORS[detail.status], fontSize: "0.55em", verticalAlign: "middle" }} data-testid="status">
          {statusLabel}
        </span>
      </h1>
      <p style={{ color: "#666", marginTop: 0 }}>
        {detail.vendor ? `${detail.vendor.business_name} (${detail.vendor.status})` : "Vendor missing"} ·{" "}
        {formatPrice(detail.price, detail.currency)} · {detail.location ?? "no location"}
      </p>

      {notice && (
        <div
          role={notice.kind === "error" ? "alert" : "status"}
          data-testid={notice.kind === "error" ? "notice-error" : "notice-ok"}
          style={{
            ...card,
            background: notice.kind === "error" ? "#fef2f2" : "#f0fdf4",
            borderColor: notice.kind === "error" ? "#fecaca" : "#bbf7d0",
          }}
        >
          {notice.text}
          {notice.problems && notice.problems.length > 0 && (
            <ul style={{ margin: "6px 0 0" }}>
              {notice.problems.map((p, i) => (
                <li key={i}>
                  <strong>{p.field}</strong>: {p.message}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(320px, 1fr))", gap: 16 }}>
        <section style={card}>
          <h3 style={{ marginTop: 0 }}>Marketplace fields</h3>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ fontSize: "0.8em", color: "#6b7280", textAlign: "left" }}>
                <th></th>
                <th>Listing (editable)</th>
                <th>Source evidence</th>
              </tr>
            </thead>
            <tbody>
              {field("product", "Product", { evidence: sig?.product })}
              {field("category", "Category", { evidence: sig?.category })}
              {field("variant", "Variant", { evidence: sig?.variant })}
              {field("condition", "Condition", { evidence: sig?.condition })}
              {field("price", "Price", { type: "number", evidence: sig ? formatPrice(sig.price, sig.currency) : null })}
              {field("currency", "Currency", { evidence: sig?.currency })}
              {field("quantity", "Quantity", { type: "number", evidence: sig?.quantity })}
              {field("location", "Location", { evidence: sig?.location ?? (detail.vendor?.primary_location ? `vendor: ${detail.vendor.primary_location}` : null) })}
              {field("description", "Description")}
            </tbody>
          </table>
          {editable ? (
            <div style={{ marginTop: 10, display: "flex", gap: 8 }}>
              <button onClick={save} disabled={!dirty || busy} data-testid="save">
                Save changes
              </button>
              <button onClick={() => reset(detail)} disabled={!dirty || busy}>
                Discard
              </button>
            </div>
          ) : (
            <p style={{ color: "#6b7280", fontSize: "0.85em" }}>This listing is {statusLabel} and can no longer be edited.</p>
          )}
        </section>

        <section style={card}>
          <h3 style={{ marginTop: 0 }}>Publication</h3>
          <table style={{ fontSize: "0.9em" }}>
            <tbody>
              <tr><td style={{ color: "#555", paddingRight: 8 }}>Status</td><td>{statusLabel}</td></tr>
              <tr><td style={{ color: "#555", paddingRight: 8 }}>Published</td><td data-testid="published-at">{detail.published ? formatDate(detail.published_at) : "no"}</td></tr>
              {detail.rejected && (
                <tr><td style={{ color: "#555", paddingRight: 8 }}>Rejected</td><td>{formatDate(detail.rejected_at)}{detail.rejection_reason ? ` — ${detail.rejection_reason}` : ""}</td></tr>
              )}
              <tr><td style={{ color: "#555", paddingRight: 8 }}>Created</td><td>{formatDate(detail.created_at)}</td></tr>
              <tr><td style={{ color: "#555", paddingRight: 8 }}>Verified</td><td>{formatDate(detail.last_verified_at)}</td></tr>
              <tr><td style={{ color: "#555", paddingRight: 8 }}>Expires</td><td>{formatDate(detail.expires_at)}</td></tr>
            </tbody>
          </table>

          {detail.status === "DRAFT" && (
            <div style={{ marginTop: 12 }}>
              {detail.publish_blockers.length > 0 && (
                <div data-testid="blockers" style={{ color: "#b45309", fontSize: "0.9em" }}>
                  Cannot publish yet:
                  <ul style={{ margin: "4px 0" }}>
                    {detail.publish_blockers.map((b, i) => (
                      <li key={i}>
                        <strong>{b.field}</strong>: {b.message}
                      </li>
                    ))}
                  </ul>
                </div>
              )}
              {dirty && <p style={{ fontSize: "0.85em", color: "#6b7280" }}>Save or discard your changes before publishing.</p>}
              <button
                onClick={publish}
                disabled={busy || dirty || detail.publish_blockers.length > 0}
                data-testid="publish"
                style={{ background: "#15803d", color: "white", border: 0, padding: "6px 14px", borderRadius: 4, cursor: "pointer" }}
              >
                Publish
              </button>
            </div>
          )}

          {editable && (
            <div style={{ marginTop: 16, borderTop: "1px solid #eee", paddingTop: 12 }}>
              <label htmlFor="reject-reason" style={{ fontSize: "0.9em" }}>
                {detail.status === "ACTIVE" ? "Withdraw (reject) this published listing" : "Reject this draft"} — reason (optional)
              </label>
              <textarea
                id="reject-reason"
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                maxLength={500}
                rows={2}
                style={{ width: "100%", boxSizing: "border-box", marginTop: 4 }}
              />
              <button onClick={reject} disabled={busy} data-testid="reject" style={{ color: "#b91c1c" }}>
                Reject
              </button>
              <p style={{ fontSize: "0.8em", color: "#6b7280" }}>
                Rejection archives the listing; it cannot be republished. The vendor&apos;s messages, the signal and the media
                are kept.
              </p>
            </div>
          )}
        </section>
      </div>

      {detail.eligibility && <LifecyclePanel el={detail.eligibility} />}

      {detail.eligibility && (detail.status === "DRAFT" || detail.status === "ACTIVE" || detail.status === "PAUSED") &&
        !detail.eligibility.lifecycle.expired && (
          <section style={card}>
            <h3 style={{ marginTop: 0 }}>Verify availability</h3>
            <p style={{ fontSize: "0.85em", color: "#6b7280", marginTop: 0 }}>
              Only after you confirmed with the vendor that the item is still available. This refreshes the
              verification time; it does not extend the lifecycle.
            </p>
            <input
              aria-label="Verification note"
              placeholder="How you confirmed (optional), e.g. called the vendor"
              value={verifyNote}
              onChange={(e) => setVerifyNote(e.target.value)}
              maxLength={500}
              style={{ padding: "4px 6px", width: "100%", maxWidth: 420, boxSizing: "border-box" }}
            />{" "}
            <button onClick={verify} disabled={busy || dirty} data-testid="verify">
              Mark verified
            </button>
          </section>
        )}

      <section style={card}>
        <h3 style={{ marginTop: 0 }}>Media ({order.length})</h3>
        {order.length === 0 && <p style={{ color: "#6b7280" }}>This listing has no media.</p>}
        <div style={{ display: "flex", gap: 12, flexWrap: "wrap" }} data-testid="media-list">
          {order.map((assetId, i) => {
            const m = mediaById[assetId];
            if (!m) return null;
            const isPrimary = primary ? primary === assetId : false;
            return (
              <div key={assetId} style={{ width: 140, fontSize: "0.8em" }} data-testid="media-item">
                <ListingMedia listingId={id} assetId={assetId} type={m.type} size={140} available={m.available} reloadKey={tokenTick} />
                <div style={{ marginTop: 4 }}>
                  #{i + 1} · {m.type}
                  {!m.available && <span style={{ color: "#b91c1c" }}> · {m.status}</span>}
                  {isPrimary && <strong style={{ color: "#15803d" }}> · PRIMARY</strong>}
                </div>
                {editable && (
                  <div style={{ display: "flex", gap: 4, marginTop: 4, flexWrap: "wrap" }}>
                    <button onClick={() => move(i, -1)} disabled={i === 0} aria-label="Move left">←</button>
                    <button onClick={() => move(i, 1)} disabled={i === order.length - 1} aria-label="Move right">→</button>
                    {m.type === "image" && !isPrimary && (
                      <button onClick={() => setPrimary(assetId)} data-testid="make-primary">
                        Make primary
                      </button>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </section>

      <section style={card}>
        <h3 style={{ marginTop: 0 }}>Provenance</h3>
        <table style={{ fontSize: "0.9em" }}>
          <tbody>
            <tr><td style={{ color: "#555", paddingRight: 8 }}>Listing</td><td style={{ fontFamily: "monospace" }}>{detail.public_reference} · {detail.id}</td></tr>
            <tr><td style={{ color: "#555", paddingRight: 8 }}>Vendor</td><td>{detail.vendor ? `${detail.vendor.business_name} · ${detail.vendor.id}` : detail.vendor_id}</td></tr>
            <tr><td style={{ color: "#555", paddingRight: 8 }}>Supply signal</td><td style={{ fontFamily: "monospace" }} data-testid="signal-id">{detail.supply_signal_id ?? "—"}</td></tr>
            {sig && (
              <>
                <tr><td style={{ color: "#555", paddingRight: 8 }}>Signal received</td><td>{formatDate(sig.created_at)} · {sig.availability_status} · confidence {(sig.confidence * 100).toFixed(0)}%</td></tr>
                <tr><td style={{ color: "#555", paddingRight: 8, verticalAlign: "top" }}>Vendor wrote</td><td style={{ whiteSpace: "pre-wrap" }}>{sig.raw_message || "—"}</td></tr>
              </>
            )}
          </tbody>
        </table>
        <h4>Source messages</h4>
        {detail.source_messages.length === 0 ? (
          <p style={{ color: "#6b7280" }}>No source messages recorded.</p>
        ) : (
          <table style={{ fontSize: "0.85em", borderCollapse: "collapse", width: "100%" }} data-testid="source-messages">
            <thead>
              <tr style={{ textAlign: "left", color: "#6b7280" }}>
                <th>#</th>
                <th>WhatsApp message</th>
                <th>Type</th>
                <th>Text / caption</th>
                <th>Media asset</th>
                <th>Received</th>
              </tr>
            </thead>
            <tbody>
              {detail.source_messages.map((s) => (
                <tr key={s.message_id} style={{ borderTop: "1px solid #eee" }}>
                  <td>{s.position + 1}</td>
                  <td style={{ fontFamily: "monospace" }}>{s.external_message_id ?? s.message_id}</td>
                  <td>{s.message_type}</td>
                  <td>{s.text ?? "—"}</td>
                  <td style={{ fontFamily: "monospace" }}>{s.media_asset_id ? s.media_asset_id.slice(0, 8) : "—"}</td>
                  <td>{formatDate(s.received_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}

// Phase 4A.5 operator view of the canonical eligibility policy: age,
// freshness, lifecycle, geographic scope, next expansion, expiry. All of it
// is derived by the backend at read time; nothing here is stored.
function LifecyclePanel({ el }: { el: import("@/lib/api").Eligibility }) {
  const lc = el.lifecycle;
  const fr = el.freshness;
  const geo = el.geography;
  const origin = geo.origin;
  const originText = origin.resolved
    ? [origin.community, origin.city, origin.state, origin.region].filter(Boolean).join(", ")
    : origin.raw
      ? `${origin.raw} (not resolved)`
      : "—";
  const row = (label: string, value: React.ReactNode, testid?: string) => (
    <tr>
      <td style={{ color: "#555", paddingRight: 12, whiteSpace: "nowrap", verticalAlign: "top" }}>{label}</td>
      <td data-testid={testid}>{value}</td>
    </tr>
  );
  return (
    <section style={{ border: "1px solid #e5e7eb", borderRadius: 6, padding: 12, marginBottom: 16 }} data-testid="lifecycle-panel">
      <h3 style={{ marginTop: 0 }}>Lifecycle &amp; eligibility</h3>
      <table style={{ fontSize: "0.9em" }}>
        <tbody>
          {row(
            "Eligible now",
            el.eligible ? (
              <strong style={{ color: "#15803d" }}>yes</strong>
            ) : (
              <span>
                <strong style={{ color: "#b91c1c" }}>no</strong> — {el.reasons.join(", ")}
              </span>
            ),
            "elig-eligible",
          )}
          {row(
            "Lifecycle",
            lc.state === "LIVE"
              ? `Day ${lc.day} / ${lc.max_days}`
              : lc.state === "EXPIRED"
                ? `Expired (day ${lc.day} / ${lc.max_days}) — kept as history`
                : lc.state === "NOT_PUBLISHED"
                  ? "Not started (starts at publication)"
                  : "Closed",
            "elig-lifecycle",
          )}
          {row(
            "Freshness",
            <span>
              <strong style={{ color: FRESHNESS_COLORS[fr.state] }}>{fr.state}</strong>
              {fr.verified_at ? ` — verified ${relativeDays(fr.verified_at)}` : ""}
              {fr.state === "STALE" ? " · needs vendor reconfirmation before telling a buyer it is available" : ""}
            </span>,
            "elig-freshness",
          )}
          {row("Search scope", `${geoLabel(geo.stage)}${geo.levels.length ? ` (${geo.levels.join(", ")})` : ""}`, "elig-scope")}
          {row("Origin", originText)}
          {geo.next_stage &&
            row(
              "Next change",
              geo.next_stage === "EXPIRED"
                ? `Expires ${relativeDays(geo.next_expansion_at)}`
                : `${geoLabel(geo.next_stage)} ${relativeDays(geo.next_expansion_at)}`,
              "elig-next",
            )}
          {row("Lifecycle ends", formatDate(lc.expires_at), "elig-expires")}
          {el.notes.length > 0 && row("Notes", el.notes.join(", "))}
        </tbody>
      </table>
    </section>
  );
}
