import type { ButtonHTMLAttributes, CSSProperties } from "react";

export type ButtonVariant = "primary" | "ghost" | "danger";
export type ButtonSize = "sm" | "md";

export interface ClawButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
}

const base: CSSProperties = {
  fontFamily: "var(--claw-font)",
  border: "1px solid transparent",
  borderRadius: "var(--claw-radius-sm)",
  cursor: "pointer",
  fontWeight: 500,
  transition: "opacity 120ms ease, background 120ms ease",
};

const variants: Record<ButtonVariant, CSSProperties> = {
  primary: { background: "var(--claw-accent)", color: "#fff" },
  ghost: {
    background: "transparent",
    color: "var(--claw-text)",
    borderColor: "var(--claw-border-strong)",
  },
  danger: { background: "var(--claw-danger)", color: "#fff" },
};

const sizes: Record<ButtonSize, CSSProperties> = {
  sm: { padding: "4px 10px", fontSize: 12 },
  md: { padding: "8px 16px", fontSize: 14 },
};

export function ClawButton({ variant = "primary", size = "md", style, disabled, ...rest }: ClawButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled}
      style={{ ...base, ...variants[variant], ...sizes[size], opacity: disabled ? 0.5 : 1, ...style }}
    />
  );
}
