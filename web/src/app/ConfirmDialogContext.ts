import { createContext, useContext } from "react";

export type ConfirmDialogOptions = {
  title: string;
  message: string;
  confirmText?: string;
  cancelText?: string;
};

export type ConfirmDialogContextValue = {
  confirm: (options: ConfirmDialogOptions) => Promise<boolean>;
};

export const ConfirmDialogContext = createContext<ConfirmDialogContextValue | null>(null);

export function useConfirm(): ConfirmDialogContextValue["confirm"] {
  const value = useContext(ConfirmDialogContext);
  if (!value) throw new Error("useConfirm must be used within ConfirmDialogProvider");
  return value.confirm;
}
