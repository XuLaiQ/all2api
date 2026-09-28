import { ChevronLeft, ChevronRight } from "lucide-react";

type PaginationProps = {
  page: number;
  totalPages: number;
  total?: number;
  loading?: boolean;
  onPageChange: (page: number) => void;
  className?: string;
};

function number(value: number): string {
  return new Intl.NumberFormat("zh-CN").format(value);
}

export function Pagination({
  page,
  totalPages,
  total,
  loading = false,
  onPageChange,
  className = "",
}: PaginationProps) {
  const lastPage = Math.max(totalPages, 1);
  const currentPage = Math.min(Math.max(page, 1), lastPage);
  const summary = total === undefined
    ? `第 ${currentPage} / ${lastPage} 页`
    : `第 ${currentPage} / ${lastPage} 页 · 共 ${number(total)} 条`;

  return (
    <nav className={`log-pagination common-pagination ${className}`.trim()} aria-label="分页">
      <span>{summary}</span>
      <div className="common-pagination-actions">
        <button
          type="button"
          className="common-pagination-button"
          aria-label="上一页"
          title="上一页"
          disabled={loading || currentPage <= 1}
          onClick={() => onPageChange(currentPage - 1)}
        >
          <ChevronLeft size={16} aria-hidden="true" />
        </button>
        <button
          type="button"
          className="common-pagination-button"
          aria-label="下一页"
          title="下一页"
          disabled={loading || currentPage >= lastPage}
          onClick={() => onPageChange(currentPage + 1)}
        >
          <ChevronRight size={16} aria-hidden="true" />
        </button>
      </div>
    </nav>
  );
}
