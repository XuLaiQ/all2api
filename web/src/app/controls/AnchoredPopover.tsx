import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";

interface AnchoredPopoverProps {
  open: boolean;
  anchorRef: { readonly current: HTMLElement | null };
  onClose: () => void;
  children: ReactNode;
  className?: string;
  /** Fixed panel width in px; defaults to the anchor width, growing for long content. */
  width?: number;
}

interface Position {
  top: number;
  left: number;
  width: number;
}

/**
 * Portal-rendered floating layer positioned next to an anchor element.
 * Escapes overflow:auto containers (modals, table wraps), flips above the
 * anchor when space is tight, and repositions on scroll/resize.
 */
export function AnchoredPopover({
  open,
  anchorRef,
  onClose,
  children,
  className = "",
  width,
}: AnchoredPopoverProps) {
  const panelRef = useRef<HTMLDivElement | null>(null);
  const [position, setPosition] = useState<Position | null>(null);

  useLayoutEffect(() => {
    if (!open) return;

    const compute = () => {
      const anchor = anchorRef.current;
      const panel = panelRef.current;
      if (!anchor || !panel) return;
      const rect = anchor.getBoundingClientRect();
      const margin = 6;
      const intrinsicWidth = panel.offsetWidth;
      const desiredWidth = width ?? Math.min(340, Math.max(rect.width, intrinsicWidth));
      const panelHeight = panel.offsetHeight;
      const spaceBelow = window.innerHeight - rect.bottom - margin;
      const spaceAbove = rect.top - margin;
      const placeAbove = spaceBelow < panelHeight + margin && spaceAbove > spaceBelow;
      const top = placeAbove
        ? Math.max(margin, rect.top - panelHeight - margin)
        : rect.bottom + margin;
      const left = Math.min(
        Math.max(margin, rect.left),
        Math.max(margin, window.innerWidth - desiredWidth - margin),
      );
      setPosition({ top, left, width: desiredWidth });
    };

    compute();
    window.addEventListener("resize", compute);
    window.addEventListener("scroll", compute, true);
    return () => {
      window.removeEventListener("resize", compute);
      window.removeEventListener("scroll", compute, true);
    };
  }, [open, anchorRef, width]);

  useLayoutEffect(() => {
    if (!open) return;

    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target as Node | null;
      if (!target) return;
      if (panelRef.current?.contains(target)) return;
      if (anchorRef.current?.contains(target)) return;
      onClose();
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.stopPropagation();
        onClose();
      }
    };

    document.addEventListener("pointerdown", handlePointerDown, true);
    document.addEventListener("keydown", handleKeyDown, true);
    return () => {
      document.removeEventListener("pointerdown", handlePointerDown, true);
      document.removeEventListener("keydown", handleKeyDown, true);
    };
  }, [open, anchorRef, onClose]);

  if (!open) return null;

  return createPortal(
    <div
      ref={panelRef}
      className={`ctrl-popover ${className}`.trim()}
      style={
        position
          ? { top: position.top, left: position.left, width: position.width }
          : { top: -9999, left: -9999, visibility: "hidden" }
      }
    >
      {children}
    </div>,
    document.body,
  );
}
