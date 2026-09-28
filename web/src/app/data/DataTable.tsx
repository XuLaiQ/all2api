import type { ReactNode } from "react";

type DataTableProps = {
  children: ReactNode;
  className?: string;
  wrapperClassName?: string;
  ariaLabel?: string;
};

/** Shared table surface; feature pages own only their columns and rows. */
export function DataTable({
  children,
  className = "",
  wrapperClassName = "",
  ariaLabel,
}: DataTableProps) {
  return (
    <div className={`table-wrap ${wrapperClassName}`.trim()}>
      <table className={`data-table ${className}`.trim()} aria-label={ariaLabel}>
        {children}
      </table>
    </div>
  );
}

export function TableState({
  children,
  live = false,
}: {
  children: ReactNode;
  live?: boolean;
}) {
  return (
    <div className="table-state" aria-live={live ? "polite" : undefined}>
      {children}
    </div>
  );
}
