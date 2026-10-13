"use client";

import { useEffect, useState } from "react";
import { createColumnHelper } from "@tanstack/react-table";
import DataTable, { Pager, type Features } from "@/components/DataTable";
import { getJobSummary, listJobs, type JobDTO, type JobSummary } from "@/lib/api";

// Reuse-audit R1: the durable job queue (deadlines, retries, recovery, media
// downloads...). The summary shows backlog and failures per job type; an
// overdue PENDING job means the worker is stuck or behind. Read-only: Majo
// retries and recovers on its own; failures that need a person also appear
// as exceptions on the Autonomy page.

const PAGE = 50;
const STATUS_COLORS: Record<string, string> = { FAILED: "crimson", RUNNING: "#3730a3", PENDING: "#b45309", SUCCEEDED: "#15803d" };

function ago(s: number): string {
  if (s <= 0) return "—";
  if (s < 120) return `${s}s`;
  if (s < 7200) return `${Math.round(s / 60)} min`;
  return `${Math.round(s / 3600)} h`;
}

const col = createColumnHelper<Features, JobDTO>();
const columns = col.columns([
  col.accessor("job_type", { header: "Type", cell: (c) => <code>{c.getValue()}</code> }),
  col.accessor("status", { header: "Status", cell: (c) => <strong style={{ color: STATUS_COLORS[c.getValue()] }}>{c.getValue()}</strong> }),
  col.accessor((j) => j.attempts, { id: "attempts", header: "Attempts", cell: (c) => `${c.getValue()} / ${c.row.original.max_attempts}` }),
  col.accessor((j) => new Date(j.run_at), { id: "run_at", header: "Run at", sortFn: "datetime", cell: (c) => c.getValue().toLocaleString() }),
  col.accessor((j) => new Date(j.updated_at), { id: "updated", header: "Updated", sortFn: "datetime", cell: (c) => c.getValue().toLocaleString() }),
  col.accessor((j) => j.last_error ?? "", {
    id: "error",
    header: "Last error",
    enableSorting: false,
    cell: (c) => <span style={{ color: "#555", fontSize: "0.85em", wordBreak: "break-word" }}>{c.getValue()}</span>,
  }),
  col.accessor("idempotency_key", {
    header: "Key",
    enableSorting: false,
    cell: (c) => <span style={{ color: "#888", fontSize: "0.8em" }}>{c.getValue().slice(0, 40)}</span>,
  }),
]);

export default function JobsPage() {
  const [summary, setSummary] = useState<JobSummary[]>([]);
  const [status, setStatus] = useState("FAILED");
  const [jobType, setJobType] = useState("");
  const [offset, setOffset] = useState(0);
  const [rows, setRows] = useState<JobDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    getJobSummary().then(setSummary).catch((e) => setError(e instanceof Error ? e.message : "unknown error"));
  }, []);

  useEffect(() => {
    listJobs(status, jobType, PAGE, offset)
      .then((p) => {
        setRows(p.rows);
        setTotal(p.total);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : "unknown error"))
      .finally(() => setLoaded(true));
  }, [status, jobType, offset]);

  const cell: React.CSSProperties = { padding: "4px 10px", borderBottom: "1px solid #eee", textAlign: "right" };
  return (
    <div>
      <h1>Jobs</h1>
      <p style={{ color: "#666" }}>Majo&apos;s durable background work. Majo retries and recovers on its own; this page is for seeing that it does.</p>
      {error && <p style={{ color: "crimson" }}>Could not load jobs: {error}</p>}
      {summary.length > 0 && (
        <table aria-label="Jobs by type" style={{ borderCollapse: "collapse", fontSize: "0.9em", marginBottom: 16 }}>
          <thead>
            <tr>
              {["Type", "Pending", "Running", "Succeeded", "Failed", "Oldest overdue"].map((h) => (
                <th key={h} style={{ ...cell, textAlign: h === "Type" ? "left" : "right", borderBottom: "2px solid #ddd" }}>
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {summary.map((s) => (
              <tr key={s.job_type}>
                <td style={{ ...cell, textAlign: "left" }}>
                  <button style={{ all: "unset", cursor: "pointer", textDecoration: "underline" }} onClick={() => { setJobType(s.job_type); setOffset(0); }}>
                    <code>{s.job_type}</code>
                  </button>
                </td>
                <td style={cell}>{s.pending}</td>
                <td style={cell}>{s.running}</td>
                <td style={cell}>{s.succeeded}</td>
                <td style={{ ...cell, color: s.failed > 0 ? "crimson" : undefined, fontWeight: s.failed > 0 ? 600 : undefined }}>{s.failed}</td>
                <td style={{ ...cell, color: s.overdue_seconds > 300 ? "crimson" : undefined }}>{ago(s.overdue_seconds)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div style={{ display: "flex", gap: 12, fontSize: "0.9em", alignItems: "center" }}>
        <label>
          Status{" "}
          <select aria-label="Job status" value={status} onChange={(e) => { setStatus(e.target.value); setOffset(0); }}>
            {["", "FAILED", "PENDING", "RUNNING", "SUCCEEDED"].map((s) => (
              <option key={s} value={s}>
                {s || "All"}
              </option>
            ))}
          </select>
        </label>
        {jobType && (
          <span>
            Type <code>{jobType}</code>{" "}
            <button onClick={() => { setJobType(""); setOffset(0); }}>clear</button>
          </span>
        )}
      </div>
      {loaded && !error && (
        <>
          <Pager offset={offset} limit={PAGE} total={total} onChange={setOffset} />
          <DataTable label="Jobs" columns={columns} data={rows} empty="No jobs match." />
        </>
      )}
    </div>
  );
}
