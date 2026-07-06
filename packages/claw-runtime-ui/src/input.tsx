import type { InputHTMLAttributes } from "react";

export interface ClawInputProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: string;
}

export function ClawInput({ label, style, id, className, ...rest }: ClawInputProps) {
  const inputId = id ?? `claw-input-${rest.name ?? "field"}`;
  return (
    <label htmlFor={inputId} style={{ display: "flex", flexDirection: "column", gap: 4, fontFamily: "var(--claw-font)" }}>
      {label ? (
        <span style={{ fontSize: 12, color: "var(--claw-text-secondary)" }}>{label}</span>
      ) : null}
      <input
        id={inputId}
        {...rest}
        className={className ? `claw-field ${className}` : "claw-field"}
        style={{ padding: "8px 12px", ...style }}
      />
    </label>
  );
}
