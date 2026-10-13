"use client";

import { useState } from "react";
import {
  createSortedRowModel,
  rowSortingFeature,
  sortFn_alphanumeric,
  sortFn_basic,
  sortFn_datetime,
  tableFeatures,
  useTable,
  type ColumnDef,
  type RowData,
  type SortingState,
} from "@tanstack/react-table";

// Shared table setup for the operator dashboard (reuse-audit R1: TanStack
// Table, headless). Rows come one server page at a time; sorting applies to
// the loaded page. Styling stays inline, like the rest of the dashboard.
export const features = tableFeatures({
  rowSortingFeature,
  sortedRowModel: createSortedRowModel(),
  sortFns: { alphanumeric: sortFn_alphanumeric, basic: sortFn_basic, datetime: sortFn_datetime },
});
export type Features = typeof features;

const th: React.CSSProperties = { textAlign: "left", padding: "6px 10px", borderBottom: "2px solid #ddd", whiteSpace: "nowrap" };
const td: React.CSSProperties = { padding: "6px 10px", borderBottom: "1px solid #eee", verticalAlign: "top" };

export default function DataTable<T extends RowData>({
  columns,
  data,
  empty = "Nothing to show.",
  label,
}: {
  columns: ColumnDef<Features, T, any>[];
  data: T[];
  empty?: string;
  label: string;
}) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const table = useTable({ features, columns, data, state: { sorting }, onSortingChange: setSorting });
  if (data.length === 0) return <p style={{ color: "#666" }}>{empty}</p>;
  return (
    <table aria-label={label} style={{ borderCollapse: "collapse", width: "100%", fontSize: "0.9em" }}>
      <thead>
        {table.getHeaderGroups().map((group) => (
          <tr key={group.id}>
            {group.headers.map((header) => {
              const dir = header.column.getIsSorted();
              const canSort = header.column.getCanSort();
              return (
                <th key={header.id} style={th} aria-sort={dir === "asc" ? "ascending" : dir === "desc" ? "descending" : "none"}>
                  {header.isPlaceholder ? null : canSort ? (
                    <button
                      onClick={header.column.getToggleSortingHandler()}
                      style={{ all: "unset", cursor: "pointer", fontWeight: 600 }}
                      title="Sort this page"
                    >
                      <table.FlexRender header={header} />
                      {dir === "asc" ? " ▲" : dir === "desc" ? " ▼" : ""}
                    </button>
                  ) : (
                    <table.FlexRender header={header} />
                  )}
                </th>
              );
            })}
          </tr>
        ))}
      </thead>
      <tbody>
        {table.getRowModel().rows.map((row) => (
          <tr key={row.id}>
            {row.getAllCells().map((cell) => (
              <td key={cell.id} style={td}>
                <table.FlexRender cell={cell} />
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// Pager for server-side pages (limit/offset with a total).
export function Pager({
  offset,
  limit,
  total,
  onChange,
}: {
  offset: number;
  limit: number;
  total: number;
  onChange: (offset: number) => void;
}) {
  const from = total === 0 ? 0 : offset + 1;
  const to = Math.min(offset + limit, total);
  return (
    <div style={{ display: "flex", gap: 8, alignItems: "center", margin: "8px 0", fontSize: "0.9em" }}>
      <button onClick={() => onChange(Math.max(0, offset - limit))} disabled={offset === 0}>
        ← Newer
      </button>
      <span>
        {from}–{to} of {total}
      </span>
      <button onClick={() => onChange(offset + limit)} disabled={offset + limit >= total}>
        Older →
      </button>
    </div>
  );
}
