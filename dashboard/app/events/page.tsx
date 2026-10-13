"use client";

import { useEffect, useState } from "react";
import { createColumnHelper } from "@tanstack/react-table";
import DataTable, { Pager, type Features } from "@/components/DataTable";
import { listEvents, listEventTypes, type TimelineEvent } from "@/lib/api";

// Reuse-audit R1: the global event timeline — every business event Majo
// recorded, newest first, with who caused it (SYSTEM, a buyer/vendor, or a
// named operator). Read-only; filters narrow by type, entity and actor.

const PAGE = 50;
const ACTORS = ["", "SYSTEM", "OPERATOR", "BUYER", "VENDOR"];
const ENTITY_LINKS: Record<string, string> = { buyer_request: "/requests/", supply_listing: "/offers/" };

function short(id: string | null): string {
  return id ? id.slice(0, 8) : "—";
}

function payloadSummary(p: Record<string, unknown>): string {
  const s = JSON.stringify(p ?? {});
  return s === "{}" ? "" : s.length > 160 ? s.slice(0, 157) + "…" : s;
}

const col = createColumnHelper<Features, TimelineEvent>();
const columns = col.columns([
  col.accessor((e) => new Date(e.created_at), {
    id: "when",
    header: "When",
    sortFn: "datetime",
    cell: (c) => <span style={{ whiteSpace: "nowrap" }}>{c.getValue().toLocaleString()}</span>,
  }),
  col.accessor("event_type", { header: "Event", cell: (c) => <code>{c.getValue()}</code> }),
  col.accessor("entity_type", {
    header: "Entity",
    cell: (c) => {
      const e = c.row.original;
      const link = e.entity_id && ENTITY_LINKS[e.entity_type];
      return (
        <span>
          {e.entity_type}{" "}
          {link ? <a href={link + e.entity_id}>{short(e.entity_id)}</a> : <span style={{ color: "#666" }}>{short(e.entity_id)}</span>}
        </span>
      );
    },
  }),
  col.accessor((e) => (e.actor_name ? `${e.actor_type} · ${e.actor_name}` : e.actor_type), { id: "actor", header: "Actor" }),
  col.accessor((e) => payloadSummary(e.payload), {
    id: "details",
    header: "Details",
    enableSorting: false,
    cell: (c) => <span style={{ color: "#555", fontSize: "0.85em", wordBreak: "break-word" }}>{c.getValue()}</span>,
  }),
]);

export default function EventsPage() {
  const [types, setTypes] = useState<string[]>([]);
  const [eventType, setEventType] = useState("");
  const [actor, setActor] = useState("");
  const [entityId, setEntityId] = useState("");
  const [offset, setOffset] = useState(0);
  const [rows, setRows] = useState<TimelineEvent[]>([]);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    listEventTypes().then(setTypes).catch(() => setTypes([]));
  }, []);

  useEffect(() => {
    const id = entityId.trim();
    if (id && !/^[0-9a-f-]{36}$/i.test(id)) return;
    listEvents({ event_type: eventType, actor_type: actor, entity_id: id }, PAGE, offset)
      .then((p) => {
        setRows(p.rows);
        setTotal(p.total);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : "unknown error"))
      .finally(() => setLoaded(true));
  }, [eventType, actor, entityId, offset]);

  const reset = (f: () => void) => {
    f();
    setOffset(0);
  };

  return (
    <div>
      <h1>Events</h1>
      <p style={{ color: "#666" }}>Every business event Majo recorded, newest first, and who caused it.</p>
      <div style={{ display: "flex", gap: 12, flexWrap: "wrap", fontSize: "0.9em" }}>
        <label>
          Event{" "}
          <select aria-label="Event type" value={eventType} onChange={(e) => reset(() => setEventType(e.target.value))}>
            <option value="">All</option>
            {types.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </label>
        <label>
          Actor{" "}
          <select aria-label="Actor type" value={actor} onChange={(e) => reset(() => setActor(e.target.value))}>
            {ACTORS.map((a) => (
              <option key={a} value={a}>
                {a || "All"}
              </option>
            ))}
          </select>
        </label>
        <label>
          Entity ID{" "}
          <input
            aria-label="Entity ID"
            placeholder="full UUID"
            value={entityId}
            onChange={(e) => reset(() => setEntityId(e.target.value))}
            style={{ width: 290 }}
          />
        </label>
      </div>
      {error && <p style={{ color: "crimson" }}>Could not load events: {error}</p>}
      {loaded && !error && (
        <>
          <Pager offset={offset} limit={PAGE} total={total} onChange={setOffset} />
          <DataTable label="Event timeline" columns={columns} data={rows} empty="No events match." />
        </>
      )}
    </div>
  );
}
