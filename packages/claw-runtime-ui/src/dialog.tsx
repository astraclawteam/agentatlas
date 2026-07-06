import type { ReactNode } from "react";
import { ClawButton } from "./button";

export interface ClawDialogProps {
  open: boolean;
  title: string;
  children: ReactNode;
  onClose: () => void;
  footer?: ReactNode;
}

export function ClawDialog({ open, title, children, onClose, footer }: ClawDialogProps) {
  if (!open) return null;
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={title}
      style={{
        position: "fixed",
        inset: 0,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        background: "rgba(17, 24, 39, 0.4)",
        zIndex: 1000,
        fontFamily: "var(--claw-font)",
      }}
      onClick={onClose}
    >
      <div
        className="claw-glass"
        style={{ minWidth: 360, maxWidth: "80vw", maxHeight: "80vh", overflow: "auto", padding: 20 }}
        onClick={(e) => e.stopPropagation()}
      >
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 12 }}>
          <h3 style={{ margin: 0, fontSize: 16, color: "var(--claw-text)" }}>{title}</h3>
          <ClawButton variant="ghost" size="sm" onClick={onClose} aria-label="关闭">
            ✕
          </ClawButton>
        </div>
        <div style={{ color: "var(--claw-text-secondary)", fontSize: 14 }}>{children}</div>
        {footer ? <div style={{ marginTop: 16, display: "flex", gap: 8, justifyContent: "flex-end" }}>{footer}</div> : null}
      </div>
    </div>
  );
}
