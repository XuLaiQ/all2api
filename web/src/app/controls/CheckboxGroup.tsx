import type { ReactNode } from "react";
import { Check } from "lucide-react";

export interface CheckboxOption {
  value: string;
  label: ReactNode;
  disabled?: boolean;
}

export interface CheckboxGroupProps {
  options: readonly CheckboxOption[];
  value: readonly string[];
  onChange: (value: string[]) => void;
  hint?: ReactNode;
  disabled?: boolean;
  ariaLabel?: string;
  className?: string;
}

export function CheckboxGroup({
  options,
  value,
  onChange,
  hint,
  disabled = false,
  ariaLabel,
  className = "",
}: CheckboxGroupProps) {
  return (
    <div className={`control-checkbox-group ${className}`.trim()} role="group" aria-label={ariaLabel}>
      {options.map((option) => {
        const checked = value.includes(option.value);
        const optionDisabled = disabled || option.disabled;
        return (
          <label className="control-checkbox" key={option.value}>
            <input
              type="checkbox"
              checked={checked}
              disabled={optionDisabled}
              onChange={(event) => {
                const next = event.currentTarget.checked
                  ? Array.from(new Set([...value, option.value]))
                  : value.filter((item) => item !== option.value);
                onChange(next);
              }}
            />
            <span className="control-checkbox-box" aria-hidden="true">
              {checked && <Check size={13} strokeWidth={3} />}
            </span>
            <span className="control-checkbox-label">{option.label}</span>
          </label>
        );
      })}
      {hint && <small className="control-checkbox-hint">{hint}</small>}
    </div>
  );
}
