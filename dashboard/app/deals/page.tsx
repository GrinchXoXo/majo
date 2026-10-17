"use client";

import { useEffect, useState } from "react";
import { createColumnHelper } from "@tanstack/react-table";
import DataTable, { Pager, type Features } from "@/components/DataTable";
import { getDealSummary, getMonetization, listDeals, type DealDTO, type DealSummary, type Monetization } from "@/lib/api";

// Pilot batch item 5. A deal is Majo's follow-up on a buyer's choice: some
// hours after it, both sides are asked "did you buy / did you sell?". Two
// YESes make a confirmed deal, numbered in order; the first ones are free
// (monetization Stage A). Disputes and 1-2 star ratings also appear as
// exceptions on the Autonomy page. Read-only.

const PAGE = 50;
const STATUS_COLORS: Record<string, string> = {
  CONFIRMED: "#15803d",
  DISPUTED: "crimson",
  DECLINED: "#555",
  BUYER_ONLY: "#b45309",
  VENDOR_ONLY: "#b45309",
  NO_ANSWER: "#888",
  ASKED: "#3730a3",
  PENDING: "#3730a3",
};
const naira = (n?: number) => (n === undefined || n === null ? "—" : `₦${n.toLocaleString("en-NG")}`);

const col = createColumnHelper<Features, DealDTO>();
const columns = col.columns([
  col.accessor((d) => d.deal_number ?? 0, { id: "number", header: "#", cell: (c) => (c.getValue() ? `#${c.getValue()}` : "—") }),
  col.accessor("status", { header: "Status", cell: (c) => <strong style={{ color: STATUS_COLORS[c.getValue()] }}>{c.getValue()}</strong> }),
  col.accessor("item", {
    header: "Item",
    cell: (c) => (
      <a href={`/requests/${c.row.original.buyer_request_id}`}>
        {c.getValue()}
      </a>
    ),
  }),
  col.accessor((d) => d.vendor_name ?? "", { id: "vendor", header: "Seller" }),
  col.accessor((d) => d.amount ?? 0, { id: "amount", header: "Amount", cell: (c) => naira(c.row.original.amount) }),
  col.accessor((d) => `${d.buyer_answer ?? (d.buyer_unreachable ? "unreachable" : "—")} / ${d.vendor_answer ?? (d.vendor_unreachable ? "unreachable" : "—")}`, {
    id: "answers",
    header: "Buyer / seller said",
    enableSorting: false,
  }),
  col.accessor((d) => d.buyer_rating ?? 0, {
    id: "rating",
    header: "Rating",
    cell: (c) => (c.getValue() ? <span style={{ color: c.getValue() <= 2 ? "crimson" : undefined }}>{"★".repeat(c.getValue())}</span> : "—"),
  }),
  col.accessor("free", { header: "Free", cell: (c) => (c.getValue() ? "free" : "") }),
  col.accessor("reference", { header: "Ref", enableSorting: false }),
  col.accessor((d) => new Date(d.created_at), { id: "created", header: "Chosen", sortFn: "datetime", cell: (c) => c.getValue().toLocaleString() }),
]);

function Tile({ label, value, sub, warn }: { label: string; value: string; sub?: string; warn?: boolean }) {
  return (
    <div style={{ border: "1px solid #e5e7eb", borderRadius: 8, padding: 12, minWidth: 170 }}>
      <div style={{ fontSize: "0.8em", color: "#555" }}>{label}</div>
      <div style={{ fontSize: "1.5em", fontWeight: 600, color: warn ? "crimson" : "#111" }}>{value}</div>
      {sub && <div style={{ fontSize: "0.8em", color: "#666" }}>{sub}</div>}
    </div>
  );
}

const STAGE_NAMES: Record<string, string> = {
  A: "Stage A: free pilot (unregistered)",
  B: "Stage B: CAC business name",
  C: "Stage C: company + licensed partner",
};

// Revenue lines and the condition each one waits for. Switching one on is
// a reviewed configuration change; the server refuses it before its stage.
function RevenueLines({ m }: { m: Monetization }) {
  const anyOn = m.lines.some((l) => l.on);
  return (
    <section aria-label="Revenue lines" style={{ margin: "20px 0" }}>
      <h2 style={{ fontSize: "1.1em" }}>Revenue lines</h2>
      <p style={{ color: "#666", maxWidth: 760, margin: "4px 0 8px" }}>
        {STAGE_NAMES[m.stage] ?? m.stage} · payment provider: <strong>{m.provider}</strong> ·{" "}
        {anyOn ? "some lines are on" : "nothing is charged: every line is off"}
        {m.ledger && ` · ledger entries: ${m.ledger.entries}`}
      </p>
      <table style={{ borderCollapse: "collapse", fontSize: "0.9em" }}>
        <thead>
          <tr>
            {["Line", "State", "Who pays", "Needs", "Waiting for"].map((h) => (
              <th key={h} style={{ textAlign: "left", borderBottom: "1px solid #ddd", padding: "4px 8px" }}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {m.lines.map((l) => (
            <tr key={l.line}>
              <td style={{ padding: "4px 8px" }}>
                <code>{l.line}</code>
              </td>
              <td style={{ padding: "4px 8px", color: l.on ? "#15803d" : "#888", fontWeight: 600 }}>{l.on ? "ON" : "off"}</td>
              <td style={{ padding: "4px 8px" }}>{l.requirement.payer}</td>
              <td style={{ padding: "4px 8px" }}>
                Stage {l.requirement.stage}, {l.requirement.providers.join(" or ")}
              </td>
              <td style={{ padding: "4px 8px", color: "#555", maxWidth: 420 }}>{l.requirement.gate}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

export default function DealsPage() {
  const [summary, setSummary] = useState<DealSummary | null>(null);
  const [money, setMoney] = useState<Monetization | null>(null);
  const [status, setStatus] = useState("");
  const [offset, setOffset] = useState(0);
  const [rows, setRows] = useState<DealDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    getDealSummary()
      .then(setSummary)
      .catch((e) => setError(e instanceof Error ? e.message : "unknown error"));
    getMonetization()
      .then(setMoney)
      .catch(() => setMoney(null)); // the deals view works without it
  }, []);

  useEffect(() => {
    listDeals(status, PAGE, offset)
      .then((p) => {
        setRows(p.rows);
        setTotal(p.total);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : "unknown error"))
      .finally(() => setLoaded(true));
  }, [status, offset]);

  const s = summary?.summary;
  return (
    <div>
      <title>Deals · Majo</title>
      <h1>Deals</h1>
      <p style={{ color: "#666", maxWidth: 760 }}>
        After a buyer chooses an offer, Majo introduces them to the seller and later asks both whether the sale happened. A deal counts once both
        say yes.
      </p>
      {error && <p style={{ color: "crimson" }}>Could not load deals: {error}</p>}
      {s && summary && (
        <section aria-label="Deal summary" style={{ display: "flex", flexWrap: "wrap", gap: 12, margin: "12px 0" }}>
          <Tile label="Confirmed deals" value={String(s.confirmed)} sub={`${naira(s.confirmed_value)} in sales`} />
          <Tile label="Free deals used" value={`${s.free_used} / ${s.free_limit}`} sub={`${s.free_remaining} free deals left`} />
          <Tile
            label="Average seller rating"
            value={s.average_rating === null ? "—" : `${s.average_rating} ★`}
            sub={`${s.ratings} ratings, ${s.low_ratings} low (1–2)`}
            warn={s.low_ratings > 0}
          />
          <Tile label="Disputed" value={String(s.by_status.DISPUTED ?? 0)} sub="buyer and seller disagree" warn={(s.by_status.DISPUTED ?? 0) > 0} />
          <Tile
            label="Waiting for answers"
            value={String((s.by_status.PENDING ?? 0) + (s.by_status.ASKED ?? 0))}
            sub={`asked ${summary.settings.follow_up_delay} after the choice`}
          />
        </section>
      )}
      <label style={{ fontSize: "0.9em" }}>
        Status{" "}
        <select aria-label="Deal status" value={status} onChange={(e) => { setStatus(e.target.value); setOffset(0); }}>
          {["", "PENDING", "ASKED", "CONFIRMED", "BUYER_ONLY", "VENDOR_ONLY", "DECLINED", "DISPUTED", "NO_ANSWER"].map((st) => (
            <option key={st} value={st}>
              {st || "All"}
            </option>
          ))}
        </select>
      </label>
      {money && <RevenueLines m={money} />}
      {loaded && !error && (
        <>
          <Pager offset={offset} limit={PAGE} total={total} onChange={setOffset} />
          <DataTable label="Deals" columns={columns} data={rows} empty="No deals yet. They appear when a buyer chooses an offer." />
        </>
      )}
    </div>
  );
}
