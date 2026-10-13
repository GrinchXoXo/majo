"use client";

import { useEffect, useState } from "react";
import { getOperatorToken, setOperatorToken } from "@/lib/api";

// Operator token entry, shown once in the navigation bar. Since reuse-audit R1
// every /api route needs it (a named operator token, or the shared
// OPERATOR_API_TOKEN). Kept in sessionStorage for this browser tab session
// only. Setting or clearing it reloads the page so every view refetches.
export default function OperatorToken({ onChange }: { onChange?: () => void }) {
  const changed = onChange ?? (() => window.location.reload());
  const [saved, setSaved] = useState(false);
  const [value, setValue] = useState("");

  useEffect(() => {
    setSaved(getOperatorToken() !== "");
  }, []);

  if (saved) {
    return (
      <span style={{ fontSize: "0.85em", color: "#15803d" }}>
        Operator token set{" "}
        <button
          onClick={() => {
            setOperatorToken("");
            setSaved(false);
            changed();
          }}
          style={{ fontSize: "0.85em" }}
        >
          Clear
        </button>
      </span>
    );
  }
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        setOperatorToken(value.trim());
        setValue("");
        setSaved(value.trim() !== "");
        changed();
      }}
      style={{ display: "inline-flex", gap: 6, alignItems: "center", fontSize: "0.85em" }}
    >
      <input
        type="password"
        aria-label="Operator token"
        placeholder="Operator token"
        value={value}
        onChange={(e) => setValue(e.target.value)}
        style={{ padding: "4px 6px" }}
      />
      <button type="submit">Use token</button>
    </form>
  );
}
