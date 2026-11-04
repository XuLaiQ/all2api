import { createContext, useContext } from "react";

export type ToastKind = "success" | "error" | "warning" | "info";

export type ToastItem = {
  id: number;
  kind: ToastKind;
  message: string;
};

export type ToastContextValue = {
  showToast: (kind: ToastKind, message: string) => void;
  success: (message: string) => void;
  error: (message: string) => void;
  warning: (message: string) => void;
  info: (message: string) => void;
};

export const ToastContext = createContext<ToastContextValue | null>(null);

export function useToast(): ToastContextValue {
  const value = useContext(ToastContext);
  if (!value) throw new Error("useToast must be used within ToastProvider");
  return value;
}
