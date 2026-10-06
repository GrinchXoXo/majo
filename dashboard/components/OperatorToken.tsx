"use client";

import { useEffect, useState } from "react";
import { getOperatorToken, setOperatorToken } from "@/lib/api";

// Operator token entry (Phase 4A Step 4). Listing edit/publish/reject and
// media previews require it when the backend has OPERATOR_API_TOKEN set.
// Kept in sessionStorage for this browser tab session only.
export default function OperatorToken({ onChange }: { onChange?: () => void }) {
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
            onChange?.();
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
        onChange?.();
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
