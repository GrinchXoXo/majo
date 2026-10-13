import { listRequests, type BuyerRequestDTO } from "@/lib/api";

// Per PRD 1 §25: requests appear in the dashboard. Phase 4C correction: Majo
// advances requests on its own; NEEDS_REVIEW only lingers when autonomy is
// off or an exception is open (see /autonomy), and AWAITING_BUYER means Majo
// asked the buyer a question — neither is an operator to-do list by itself.

function statusBadgeColor(status: string): string {
  switch (status) {
    case "NEEDS_REVIEW":
      return "#b45309"; // amber
    case "EXTRACTING":
      return "#6b7280"; // gray
    case "CONFIRMED":
      return "#15803d"; // green
    case "AWAITING_BUYER":
      return "#3730a3"; // indigo: Majo asked the buyer
    default:
      return "#6b7280";
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

export default async function RequestsPage() {
  let requests: BuyerRequestDTO[], error: string | null = null;

  try {
    requests = await listRequests();
  } catch (e) {
    error = e instanceof Error ? e.message : "unknown error";
    requests = [];
  }

  return (
    <div>
      <h1>Requests</h1>
      <p style={{ color: "#666" }}>
        Buyer requests, most in need of review first. Click a row to inspect, correct, or confirm.
      </p>

      {error && (
        <p style={{ color: "crimson" }}>Could not load requests: {error}</p>
      )}

      {requests.length === 0 && !error && (
        <p style={{ color: "#666" }}>No requests yet.</p>
      )}

      {requests.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse", marginTop: 16 }}>
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "2px solid #ddd" }}>
              <th style={{ padding: "8px 4px" }}>Status</th>
              <th style={{ padding: "8px 4px" }}>Product</th>
              <th style={{ padding: "8px 4px" }}>Budget</th>
              <th style={{ padding: "8px 4px" }}>Location</th>
              <th style={{ padding: "8px 4px" }}>Completeness</th>
              <th style={{ padding: "8px 4px" }}>Confidence</th>
              <th style={{ padding: "8px 4px" }}>Created</th>
            </tr>
          </thead>
          <tbody>
            {requests.map((r) => (
              <tr key={r.id} style={{ borderBottom: "1px solid #eee" }}>
                <td style={{ padding: "8px 4px" }}>
                  <a href={`/requests/${r.id}`} style={{ textDecoration: "none" }}>
                    <span
                      style={{
                        color: statusBadgeColor(r.status),
                        fontWeight: 600,
                        fontSize: "0.85em",
                      }}
                    >
                      {r.status}
                    </span>
                  </a>
                </td>
                <td style={{ padding: "8px 4px" }}>
                  <a href={`/requests/${r.id}`}>{r.product ?? <em>unknown</em>}</a>
                </td>
                <td style={{ padding: "8px 4px" }}>
                  {r.budget_min || r.budget_max
                    ? `₦${(r.budget_min ?? 0).toLocaleString()} - ₦${(r.budget_max ?? 0).toLocaleString()}`
                    : "—"}
                </td>
                <td style={{ padding: "8px 4px" }}>{r.location ?? "—"}</td>
                <td style={{ padding: "8px 4px" }}>{completenessLabel(r.completeness)}</td>
                <td style={{ padding: "8px 4px" }}>{(r.confidence * 100).toFixed(0)}%</td>
                <td style={{ padding: "8px 4px", color: "#666", fontSize: "0.85em" }}>
                  {new Date(r.created_at).toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
