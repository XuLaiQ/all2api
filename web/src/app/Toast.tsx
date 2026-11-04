import { useCallback, useMemo, useState, type ReactNode } from "react";
import { CheckCircle2, Info, TriangleAlert, X } from "lucide-react";
import { ToastContext, type ToastContextValue, type ToastItem, type ToastKind } from "./ToastContext";
import "./Toast.css";

const toastIcons = {
  success: CheckCircle2,
  error: TriangleAlert,
  warning: TriangleAlert,
  info: Info,
};

let nextToastId = 1;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([]);

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id));
  }, []);

  const showToast = useCallback((kind: ToastKind, message: string) => {
    const text = message.trim();
    if (!text) return;
    const id = nextToastId++;
    setToasts((current) => [...current.filter((toast) => toast.message !== text), { id, kind, message: text }].slice(-4));
    window.setTimeout(() => dismiss(id), kind === "error" ? 6000 : 3600);
  }, [dismiss]);

  const value = useMemo<ToastContextValue>(() => ({
    showToast,
    success: (message) => showToast("success", message),
    error: (message) => showToast("error", message),
    warning: (message) => showToast("warning", message),
    info: (message) => showToast("info", message),
  }), [showToast]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="toast-viewport" aria-live="polite" aria-atomic="false">
        {toasts.map((toast) => {
          const Icon = toastIcons[toast.kind];
          return (
            <div className={`toast toast-${toast.kind}`} key={toast.id} role={toast.kind === "error" ? "alert" : "status"}>
              <Icon size={17} aria-hidden="true" />
              <span>{toast.message}</span>
              <button type="button" className="toast-close" aria-label="关闭提示" onClick={() => dismiss(toast.id)}><X size={14} /></button>
            </div>
          );
        })}
      </div>
    </ToastContext.Provider>
  );
}
