import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { AnchoredPopover } from "./AnchoredPopover";

interface DateInputProps {
  /** YYYY-MM-DD, or "" when empty. */
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  placeholder?: string;
  id?: string;
  ariaLabel?: string;
  className?: string;
}

const WEEKDAYS = ["一", "二", "三", "四", "五", "六", "日"];
const MONTH_LABELS = Array.from({ length: 12 }, (_, index) => `${index + 1}月`);

function parseDate(value: string): Date | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  if (!match) return null;
  const year = Number(match[1]);
  const month = Number(match[2]) - 1;
  const day = Number(match[3]);
  const date = new Date(year, month, day);
  if (date.getFullYear() !== year || date.getMonth() !== month || date.getDate() !== day) {
    return null;
  }
  return date;
}

function toISO(date: Date): string {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, "0");
  const day = String(date.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear()
    && a.getMonth() === b.getMonth()
    && a.getDate() === b.getDate();
}

function addDays(date: Date, amount: number): Date {
  const next = new Date(date);
  next.setDate(next.getDate() + amount);
  return next;
}

export function DateInput({
  value,
  onChange,
  disabled = false,
  placeholder = "年 / 月 / 日",
  id,
  ariaLabel = "选择日期",
  className = "",
}: DateInputProps) {
  const [open, setOpen] = useState(false);
  const [view, setView] = useState<"days" | "months">("days");
  const today = useMemo(() => new Date(), []);
  const selected = parseDate(value);
  const [panelYear, setPanelYear] = useState(today.getFullYear());
  const [panelMonth, setPanelMonth] = useState(today.getMonth());
  const [cursor, setCursor] = useState<Date>(selected ?? today);
  const wrapperRef = useRef<HTMLDivElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);

  function openPicker() {
    if (disabled) return;
    const base = selected ?? new Date();
    setPanelYear(base.getFullYear());
    setPanelMonth(base.getMonth());
    setCursor(base);
    setView("days");
    setOpen(true);
  }

  function closeAndFocus() {
    setOpen(false);
    wrapperRef.current?.querySelector<HTMLButtonElement>(".ctrl-date-trigger")?.focus();
  }

  useLayoutEffect(() => {
    if (open) panelRef.current?.focus();
  }, [open, view]);

  const cells = useMemo(() => {
    const first = new Date(panelYear, panelMonth, 1);
    const offset = (first.getDay() + 6) % 7; // Monday first
    const start = addDays(first, -offset);
    return Array.from({ length: 42 }, (_, index) => addDays(start, index));
  }, [panelYear, panelMonth]);

  function syncViewTo(date: Date) {
    setCursor(date);
    if (date.getFullYear() !== panelYear || date.getMonth() !== panelMonth) {
      setPanelYear(date.getFullYear());
      setPanelMonth(date.getMonth());
    }
  }

  function shiftMonth(amount: number) {
    const next = new Date(panelYear, panelMonth + amount, 1);
    setPanelYear(next.getFullYear());
    setPanelMonth(next.getMonth());
  }

  function handlePanelKeyDown(event: React.KeyboardEvent<HTMLDivElement>) {
    switch (event.key) {
      case "ArrowLeft":
        event.preventDefault();
        syncViewTo(addDays(cursor, -1));
        break;
      case "ArrowRight":
        event.preventDefault();
        syncViewTo(addDays(cursor, 1));
        break;
      case "ArrowUp":
        event.preventDefault();
        syncViewTo(addDays(cursor, -7));
        break;
      case "ArrowDown":
        event.preventDefault();
        syncViewTo(addDays(cursor, 7));
        break;
      case "Enter":
        event.preventDefault();
        onChange(toISO(cursor));
        setOpen(false);
        break;
      default:
        break;
    }
  }

  return (
    <div ref={wrapperRef} className={`ctrl-date ${className}`.trim()}>
      <button
        type="button"
        id={id}
        className="ctrl-date-trigger"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={ariaLabel}
        disabled={disabled}
        onClick={() => (open ? setOpen(false) : openPicker())}
      >
        <span className={selected ? "ctrl-date-value" : "ctrl-date-value ctrl-placeholder"}>
          {selected ? value : placeholder}
        </span>
      </button>
      {selected && !disabled && (
        <button
          type="button"
          className="ctrl-date-clear"
          aria-label="清除日期"
          title="清除日期"
          onMouseDown={(event) => event.preventDefault()}
          onClick={() => {
            onChange("");
            setOpen(false);
          }}
        >
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round" aria-hidden="true">
            <path d="M6 6l12 12M18 6 6 18" />
          </svg>
        </button>
      )}
      <span className="ctrl-date-icon" aria-hidden="true">
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round">
          <rect x="3.5" y="5" width="17" height="15.5" rx="3" />
          <path d="M3.5 9.5h17M8 3v4M16 3v4" />
        </svg>
      </span>

      <AnchoredPopover open={open} anchorRef={wrapperRef} onClose={closeAndFocus} width={288}>
        <div
          ref={panelRef}
          className="ctrl-date-popover"
          role="dialog"
          aria-label="选择日期"
          tabIndex={-1}
          onKeyDown={handlePanelKeyDown}
        >
          <div className="ctrl-calendar-head">
            <button
              type="button"
              className="ctrl-calendar-title"
              onClick={() => setView(view === "days" ? "months" : "days")}
            >
              {view === "days"
                ? `${panelYear}年 ${String(panelMonth + 1).padStart(2, "0")}月`
                : `${panelYear}年`}
            </button>
            <div className="ctrl-calendar-nav">
              <button
                type="button"
                aria-label={view === "days" ? "上一个月" : "上一年"}
                onClick={() => (view === "days" ? shiftMonth(-1) : setPanelYear(panelYear - 1))}
              >
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m14.5 6-6 6 6 6" /></svg>
              </button>
              <button
                type="button"
                aria-label={view === "days" ? "下一个月" : "下一年"}
                onClick={() => (view === "days" ? shiftMonth(1) : setPanelYear(panelYear + 1))}
              >
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m9.5 6 6 6-6 6" /></svg>
              </button>
            </div>
          </div>

          {view === "days" ? (
            <>
              <div className="ctrl-calendar-weekdays">
                {WEEKDAYS.map((day) => <span key={day}>{day}</span>)}
              </div>
              <div className="ctrl-calendar-grid">
                {cells.map((date) => {
                  const outside = date.getMonth() !== panelMonth;
                  const isSelectedDay = selected !== null && sameDay(date, selected);
                  const isToday = sameDay(date, today);
                  const isCursor = sameDay(date, cursor);
                  const classes = [
                    "ctrl-calendar-day",
                    outside ? "is-outside" : "",
                    isToday ? "is-today" : "",
                    isCursor ? "is-cursor" : "",
                    isSelectedDay ? "is-selected" : "",
                  ].filter(Boolean).join(" ");
                  return (
                    <button
                      key={toISO(date)}
                      type="button"
                      className={classes}
                      onClick={() => {
                        onChange(toISO(date));
                        setOpen(false);
                      }}
                    >
                      {date.getDate()}
                    </button>
                  );
                })}
              </div>
            </>
          ) : (
            <div className="ctrl-calendar-months">
              {MONTH_LABELS.map((label, month) => {
                const isCurrentMonth = today.getFullYear() === panelYear && today.getMonth() === month;
                const isSelectedMonth = selected !== null
                  && selected.getFullYear() === panelYear
                  && selected.getMonth() === month;
                const classes = [
                  "ctrl-calendar-month",
                  isCurrentMonth ? "is-today" : "",
                  isSelectedMonth ? "is-selected" : "",
                ].filter(Boolean).join(" ");
                return (
                  <button
                    key={label}
                    type="button"
                    className={classes}
                    onClick={() => {
                      setPanelMonth(month);
                      setView("days");
                    }}
                  >
                    {label}
                  </button>
                );
              })}
            </div>
          )}

          <div className="ctrl-calendar-foot">
            <button
              type="button"
              onClick={() => {
                onChange("");
                setOpen(false);
              }}
            >
              清除
            </button>
            <button
              type="button"
              onClick={() => {
                const now = new Date();
                onChange(toISO(now));
                setPanelYear(now.getFullYear());
                setPanelMonth(now.getMonth());
                setOpen(false);
              }}
            >
              今天
            </button>
          </div>
        </div>
      </AnchoredPopover>
    </div>
  );
}
