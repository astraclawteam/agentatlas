import type { ButtonHTMLAttributes, CSSProperties } from "react";

export type ButtonVariant = "primary" | "ghost" | "danger";
export type ButtonSize = "sm" | "md";

export interface ClawButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
}

/* 视觉全部走 tokens.css 的 .claw-btn（DESIGN §5：squircle 11px，
   primary=墨底白字 hover→陶土）；这里只留尺寸。 */
const sizes: Record<ButtonSize, CSSProperties> = {
  sm: { padding: "4px 10px", fontSize: 12 },
  md: { padding: "8px 16px", fontSize: 14 },
};

export function ClawButton({ variant = "primary", size = "md", style, className, ...rest }: ClawButtonProps) {
  return (
    <button
      {...rest}
      data-variant={variant}
      className={className ? `claw-btn ${className}` : "claw-btn"}
      style={{ ...sizes[size], ...style }}
    />
  );
}
