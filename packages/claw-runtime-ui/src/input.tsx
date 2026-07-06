import type { InputHTMLAttributes } from "react";

export interface ClawInputProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: string;
}

export function ClawInput({ label, style, id, ...rest }: ClawInputProps) {
  const inputId = id ?? `claw-input-${rest.name ?? "field"}`;
  return (
    <label htmlFor={inputId} style={{ display: "flex", flexDirection: "column", gap: 4, fontFamily: "var(--claw-font)" }}>
      {label ? (
        <span style={{ fontSize: 12, color: "var(--claw-text-secondary)" }}>{label}</span>
      ) : null}
      <input
        id={inputId}
        {...rest}
        style={{
          padding: "8px 12px",
          fontSize: 14,
          color: "var(--claw-text)",
          background: "var(--claw-surface-solid)",
          border: "1px solid var(--claw-border-strong)",
          borderRadius: "var(--claw-radius-sm)",
          outline: "none",
          ...style,
        }}
      />
    </label>
  );
}
