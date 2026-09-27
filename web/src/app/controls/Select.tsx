import { useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { AnchoredPopover } from "./AnchoredPopover";

export interface SelectOption {
  value: string;
  label: ReactNode;
  disabled?: boolean;
}

export interface SelectOptionGroup {
  group: string;
  options: SelectOption[];
}

export type SelectEntry = SelectOption | SelectOptionGroup;

function isGroup(entry: SelectEntry): entry is SelectOptionGroup {
  return "group" in entry;
}

interface SelectProps {
  value: string;
  onChange: (value: string) => void;
  options: SelectEntry[];
  placeholder?: string;
  disabled?: boolean;
  required?: boolean;
  id?: string;
  ariaLabel?: string;
  className?: string;
}

export function Select({
  value,
  onChange,
  options,
  placeholder,
  disabled = false,
  required = false,
  id,
  ariaLabel,
  className = "",
}: SelectProps) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  const uid = useId().replace(/[^a-zA-Z0-9_-]/g, "");

  const flat = useMemo(() => {
    const result: SelectOption[] = [];
    for (const entry of options) {
      if (isGroup(entry)) result.push(...entry.options);
      else result.push(entry);
    }
    return result;
  }, [options]);

  const selected = flat.find((option) => option.value === value);
  const selectedIndex = Math.max(0, flat.findIndex((option) => option.value === value));

  function openMenu() {
    if (disabled) return;
    setActive(selectedIndex);
    setOpen(true);
  }

  function closeAndFocus() {
    setOpen(false);
    triggerRef.current?.focus();
  }

  function choose(option: SelectOption) {
    if (option.disabled) return;
    onChange(option.value);
    closeAndFocus();
  }

  function moveActive(delta: number) {
    if (flat.length === 0) return;
    let next = active;
    for (let i = 0; i < flat.length; i += 1) {
      next = (next + delta + flat.length) % flat.length;
      if (!flat[next].disabled) break;
    }
    setActive(next);
  }

  useLayoutEffect(() => {
    if (!open) return;
    const element = listRef.current?.querySelector<HTMLElement>(`[data-index="${active}"]`);
    element?.scrollIntoView({ block: "nearest" });
  }, [open, active]);

  function handleTriggerKeyDown(event: React.KeyboardEvent<HTMLButtonElement>) {
    if (disabled) return;
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        if (!open) openMenu();
        else moveActive(1);
        break;
      case "ArrowUp":
        event.preventDefault();
        if (!open) {
          openMenu();
          let index = flat.length - 1;
          while (index > 0 && flat[index].disabled) index -= 1;
          setActive(index);
        } else {
          moveActive(-1);
        }
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        if (!open) openMenu();
        else {
          const option = flat[active];
          if (option) choose(option);
        }
        break;
      case "Tab":
      case "Escape":
        if (open) setOpen(false);
        break;
      default:
        break;
    }
  }

  function renderOption(option: SelectOption) {
    const index = flat.indexOf(option);
    const isSelected = option.value === value;
    return (
      <button
        key={`${option.value}-${index}`}
        type="button"
        role="option"
        id={`${uid}-opt-${index}`}
        data-index={index}
        aria-selected={isSelected}
        disabled={option.disabled}
        className={
          `ctrl-select-option ${index === active ? "is-active" : ""} ${isSelected ? "is-selected" : ""}`.trim()
        }
        onMouseEnter={() => {
          if (!option.disabled) setActive(index);
        }}
        onClick={() => choose(option)}
      >
        <span className="ctrl-select-option-label">{option.label}</span>
        {isSelected && (
          <svg
            className="ctrl-select-check"
            width="14"
            height="14"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.6"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <path d="m5 12.5 4.5 4.5L19 7.5" />
          </svg>
        )}
      </button>
    );
  }

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        id={id}
        className={`ctrl-select ${open ? "is-open" : ""} ${className}`.trim()}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={ariaLabel}
        aria-required={required}
        aria-activedescendant={open ? `${uid}-opt-${active}` : undefined}
        disabled={disabled}
        onClick={() => (open ? setOpen(false) : openMenu())}
        onKeyDown={handleTriggerKeyDown}
      >
        <span className={selected ? "ctrl-select-value" : "ctrl-select-value ctrl-placeholder"}>
          {selected ? selected.label : placeholder ?? " "}
        </span>
        <svg
          className="ctrl-chevron"
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2.4"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <path d="m6 9 6 6 6-6" />
        </svg>
      </button>
      <AnchoredPopover open={open} anchorRef={triggerRef} onClose={closeAndFocus}>
        <div
          ref={listRef}
          className="ctrl-select-menu"
          role="listbox"
          aria-label={ariaLabel ?? "选项"}
        >
          {flat.length === 0 && <div className="ctrl-select-empty">暂无可选项</div>}
          {options.map((entry) => {
            if (isGroup(entry)) {
              if (entry.options.length === 0) return null;
              return (
                <div className="ctrl-select-group" key={entry.group}>
                  <div className="ctrl-select-group-label">{entry.group}</div>
                  {entry.options.map(renderOption)}
                </div>
              );
            }
            return renderOption(entry);
          })}
        </div>
      </AnchoredPopover>
    </>
  );
}
