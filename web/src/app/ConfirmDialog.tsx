import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { TriangleAlert } from "lucide-react";
import { Button } from "./controls/Button";
import { ConfirmDialogContext, type ConfirmDialogContextValue, type ConfirmDialogOptions } from "./ConfirmDialogContext";
import "./ConfirmDialog.css";

export function ConfirmDialogProvider({ children }: { children: ReactNode }) {
  const [current, setCurrent] = useState<Required<ConfirmDialogOptions> | null>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const cancelButtonRef = useRef<HTMLButtonElement>(null);
  const resolverRef = useRef<((confirmed: boolean) => void) | null>(null);

  const confirm = useCallback((options: ConfirmDialogOptions) => new Promise<boolean>((resolve) => {
    resolverRef.current?.(false);
    resolverRef.current = resolve;
    setCurrent({
      title: options.title,
      message: options.message,
      confirmText: options.confirmText ?? "确定",
      cancelText: options.cancelText ?? "取消",
    });
  }), []);

  const settle = useCallback((confirmed: boolean) => {
    const resolve = resolverRef.current;
    resolverRef.current = null;
    if (dialogRef.current?.open) dialogRef.current.close();
    setCurrent(null);
    resolve?.(confirmed);
  }, []);

  useEffect(() => {
    if (!current || !dialogRef.current) return;
    if (!dialogRef.current.open) dialogRef.current.showModal();
    cancelButtonRef.current?.focus();
  }, [current]);

  useEffect(() => () => {
    resolverRef.current?.(false);
    resolverRef.current = null;
  }, []);

  const value = useMemo<ConfirmDialogContextValue>(() => ({ confirm }), [confirm]);

  return (
    <ConfirmDialogContext.Provider value={value}>
      {children}
      {current && (
        <dialog
          ref={dialogRef}
          className="confirm-dialog"
          role="alertdialog"
          aria-modal="true"
          aria-labelledby="confirm-dialog-title"
          aria-describedby="confirm-dialog-message"
          onCancel={(event) => {
            event.preventDefault();
            settle(false);
          }}
          onClick={(event) => {
            if (event.target === event.currentTarget) settle(false);
          }}
        >
          <div className="confirm-dialog-content">
            <div className="confirm-dialog-heading">
              <span className="confirm-dialog-icon" aria-hidden="true"><TriangleAlert size={18} /></span>
              <h2 id="confirm-dialog-title">{current.title}</h2>
            </div>
            <p id="confirm-dialog-message">{current.message}</p>
            <div className="confirm-dialog-actions">
              <Button ref={cancelButtonRef} variant="secondary" size="sm" onClick={() => settle(false)}>
                {current.cancelText}
              </Button>
              <Button variant="danger" size="sm" onClick={() => settle(true)}>
                {current.confirmText}
              </Button>
            </div>
          </div>
        </dialog>
      )}
    </ConfirmDialogContext.Provider>
  );
}
