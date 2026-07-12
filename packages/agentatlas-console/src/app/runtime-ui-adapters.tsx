import { forwardRef, type ComponentPropsWithoutRef } from "react";
import { Button, Input } from "@xiaozhiclaw/runtime-ui";

type LegacyButtonProps = ComponentPropsWithoutRef<typeof Button> & {
  size?: "sm" | "md" | "lg";
  variant?: "primary" | "ghost" | "danger";
};

export const LegacyButton = forwardRef<HTMLButtonElement, LegacyButtonProps>(function LegacyButton(
  { size: _size, variant: _variant, ...props },
  ref,
) {
  return <Button ref={ref} {...props} />;
});

type LegacyInputProps = ComponentPropsWithoutRef<typeof Input> & { label?: string };

export const LegacyInput = forwardRef<HTMLInputElement, LegacyInputProps>(function LegacyInput(
  { label, id, ...props },
  ref,
) {
  if (!label) return <Input ref={ref} id={id} {...props} />;
  const inputId = id ?? `legacy-${label.replace(/\s+/g, "-")}`;
  return (
    <label htmlFor={inputId}>
      <span>{label}</span>
      <Input ref={ref} id={inputId} {...props} />
    </label>
  );
});
