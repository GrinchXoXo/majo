"use client";

import { useEffect, useState } from "react";
import { ApiError, fetchListingMedia } from "@/lib/api";

// Renders one listing media item through the backend preview endpoint
// (storage boundary; no storage keys or credentials in the browser).
// Every state is terminal: loading -> shown | unavailable | needs token.
export default function ListingMedia({
  listingId,
  assetId,
  type,
  size = 96,
  available = true,
  reloadKey = 0,
}: {
  listingId: string;
  assetId: string;
  type: string;
  size?: number;
  available?: boolean;
  reloadKey?: number;
}) {
  const [url, setUrl] = useState<string | null>(null);
  const [state, setState] = useState<"loading" | "ok" | "unavailable" | "unauthorized">(available ? "loading" : "unavailable");

  useEffect(() => {
    if (!available) {
      setState("unavailable");
      return;
    }
    let cancelled = false;
    let objectUrl: string | null = null;
    setState("loading");
    fetchListingMedia(listingId, assetId)
      .then((u) => {
        objectUrl = u;
        if (cancelled) {
          URL.revokeObjectURL(u);
          return;
        }
        setUrl(u);
        setState("ok");
      })
      .catch((e) => {
        if (!cancelled) setState(e instanceof ApiError && e.code === "UNAUTHORIZED" ? "unauthorized" : "unavailable");
      });
    return () => {
      cancelled = true;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [listingId, assetId, available, reloadKey]);

  const box: React.CSSProperties = {
    width: size,
    height: size,
    background: "#f3f4f6",
    border: "1px solid #e5e7eb",
    borderRadius: 4,
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    fontSize: "0.75em",
    color: "#6b7280",
    textAlign: "center",
    overflow: "hidden",
  };

  if (state === "ok" && url) {
    if (type === "video") {
      return (
        <video src={url} controls preload="metadata" onError={() => setState("unavailable")} style={{ ...box, objectFit: "cover" }} data-testid="media-video" />
      );
    }
    // Bytes that the browser cannot decode count as unavailable, not as a broken image.
    return <img src={url} alt="listing media" onError={() => setState("unavailable")} style={{ ...box, objectFit: "cover" }} data-testid="media-image" />;
  }
  const label = { loading: "Loading…", unavailable: "Media unavailable", unauthorized: "Token needed", ok: "" }[state];
  return (
    <div style={box} data-testid={`media-${state}`}>
      {label}
    </div>
  );
}
