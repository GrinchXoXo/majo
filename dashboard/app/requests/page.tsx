"use client";

import { useEffect, useState } from "react";
import { createColumnHelper } from "@tanstack/react-table";
import DataTable, { Pager, type Features } from "@/components/DataTable";
import { listRequestsPage, type BuyerRequestDTO } from "@/lib/api";

// Per PRD 1 §25: requests appear in the dashboard. Phase 4C correction: Majo
// advances requests on its own; NEEDS_REVIEW only lingers when autonomy is
// off or an exception is open (see /autonomy), and AWAITING_BUYER means Majo
// asked the buyer a question — neither is an operator to-do list by itself.
// Reuse-audit R1: server-paginated, TanStack Table, operator token required.

const PAGE = 50;
const STATUSES = ["", "NEEDS_REVIEW", "AWAITING_BUYER", "CONFIRMED", "EXTRACTING"];

function statusBadgeColor(status: string): string {
  switch (status) {
    case "NEEDS_REVIEW":
      return "#b45309"; // amber
    case "CONFIRMED":
      return "#15803d"; // green
    case "AWAITING_BUYER":
      return "#3730a3"; // indigo: Majo asked the buyer
    default:
      return "#6b7280"; // gray (EXTRACTING and anything new)
  }
}

function completenessLabel(c: string): string {
  switch (c) {
    case "complete":
      return "✓ complete";
    case "partially_complete":
      return "◐ partial";
    default:
      return "○ insufficient";
  }
}

const col = createColumnHelper<Features, BuyerRequestDTO>();
const columns = col.columns([
  col.accessor("status", {
    header: "Status",
    cell: (c) => (
      <a href={`/requests/${c.row.original.id}`} style={{ textDecoration: "none", color: statusBadgeColor(c.getValue()), fontWeight: 600, fontSize: "0.85em" }}>
        {c.getValue()}
      </a>
    ),
  }),
  col.accessor((r) => r.product ?? "", {
    id: "product",
    header: "Product",
    cell: (c) => <a href={`/requests/${c.row.original.id}`}>{c.getValue() || <em>unknown</em>}</a>,
  }),
  col.accessor((r) => r.budget_max ?? r.budget_min ?? 0, {
    id: "budget",
    header: "Budget",
    cell: (c) => {
      const r = c.row.original;
      return r.budget_min || r.budget_max ? `₦${(r.budget_min ?? 0).toLocaleString()} - ₦${(r.budget_max ?? 0).toLocaleString()}` : "—";
    },
  }),
  col.accessor((r) => r.location ?? "", { id: "location", header: "Location", cell: (c) => c.getValue() || "—" }),
  col.accessor("completeness", { header: "Completeness", cell: (c) => completenessLabel(c.getValue()) }),
  col.accessor("confidence", { header: "Confidence", cell: (c) => `${(c.getValue() * 100).toFixed(0)}%` }),
  col.accessor((r) => new Date(r.created_at), {
    id: "created",
    header: "Created",
    sortFn: "datetime",
    cell: (c) => <span style={{ color: "#666", fontSize: "0.9em" }}>{c.getValue().toLocaleString()}</span>,
  }),
]);

export default function RequestsPage() {
  const [status, setStatus] = useState("");
  const [offset, setOffset] = useState(0);
  const [rows, setRows] = useState<BuyerRequestDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    listRequestsPage(status, PAGE, offset)
      .then((p) => {
        setRows(p.rows);
        setTotal(p.total);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : "unknown error"))
      .finally(() => setLoaded(true));
  }, [status, offset]);

  return (
    <div>
      <title>Requests · Majo</title>
      <h1>Requests</h1>
      <p style={{ color: "#666" }}>Buyer requests, most in need of review first. Click a row to inspect, correct, or confirm.</p>
      <label style={{ fontSize: "0.9em" }}>
        Status{" "}
        <select
          value={status}
          onChange={(e) => {
            setStatus(e.target.value);
            setOffset(0);
          }}
        >
          {STATUSES.map((s) => (
            <option key={s} value={s}>
              {s || "All"}
            </option>
          ))}
        </select>
      </label>
      {error && <p style={{ color: "crimson" }}>Could not load requests: {error}</p>}
      {loaded && !error && (
        <>
          <Pager offset={offset} limit={PAGE} total={total} onChange={setOffset} />
          <DataTable label="Buyer requests" columns={columns} data={rows} empty="No requests yet." />
        </>
      )}
    </div>
  );
}
