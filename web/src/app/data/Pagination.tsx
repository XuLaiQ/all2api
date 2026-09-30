import { useEffect, useMemo, useState } from "react";
import { ChevronLeft, ChevronRight, MoreHorizontal } from "lucide-react";
import { Button } from "../controls/Button";
import { Select } from "../controls/Select";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "./pagination.constants";

type LayoutItem = "total" | "sizes" | "prev" | "pager" | "next" | "jumper";
type PageItem = number | "prev-more" | "next-more";

export type PaginationProps = {
  currentPage?: number;
  pageSize?: number;
  total: number;
  pageSizes?: readonly number[];
  pagerCount?: number;
  layout?: string;
  background?: boolean;
  small?: boolean;
  disabled?: boolean;
  hideOnSinglePage?: boolean;
  onCurrentChange: (page: number) => void;
  onSizeChange?: (pageSize: number) => void;
  className?: string;
  ariaLabel?: string;
};

function formatNumber(value: number): string {
  return new Intl.NumberFormat("zh-CN").format(value);
}

function normalizePageSize(value: number): number {
  return Number.isFinite(value) && value > 0 ? Math.floor(value) : DEFAULT_PAGE_SIZE;
}

function normalizePagerCount(value: number): number {
  const count = Number.isFinite(value) ? Math.max(5, Math.floor(value)) : 7;
  return count % 2 === 0 ? count - 1 : count;
}

function getPageItems(currentPage: number, pageCount: number, pagerCount: number): PageItem[] {
  if (pageCount <= pagerCount) {
    return Array.from({ length: pageCount }, (_, index) => index + 1);
  }

  const sideCount = Math.floor((pagerCount - 3) / 2);
  if (currentPage <= sideCount + 2) {
    return [
      ...Array.from({ length: pagerCount - 1 }, (_, index) => index + 1),
      "next-more",
      pageCount,
    ];
  }
  if (currentPage >= pageCount - sideCount - 1) {
    return [
      1,
      "prev-more",
      ...Array.from({ length: pagerCount - 1 }, (_, index) => pageCount - pagerCount + 2 + index),
    ];
  }

  return [
    1,
    "prev-more",
    ...Array.from({ length: sideCount * 2 + 1 }, (_, index) => currentPage - sideCount + index),
    "next-more",
    pageCount,
  ];
}

function parseLayout(layout: string): Array<LayoutItem | "->"> {
  const supported = new Set<LayoutItem | "->">(["total", "sizes", "prev", "pager", "next", "jumper", "->"]);
  return layout
    .split(",")
    .map((item) => item.trim())
    .filter((item): item is LayoutItem | "->" => supported.has(item as LayoutItem | "->"));
}

export function Pagination({
  currentPage = 1,
  pageSize = DEFAULT_PAGE_SIZE,
  total,
  pageSizes = PAGE_SIZE_OPTIONS,
  pagerCount = 7,
  layout = "total, sizes, prev, pager, next, jumper",
  background = false,
  small = false,
  disabled = false,
  hideOnSinglePage = false,
  onCurrentChange,
  onSizeChange,
  className = "",
  ariaLabel = "分页",
}: PaginationProps) {
  const safeTotal = Math.max(0, Number.isFinite(total) ? total : 0);
  const safePageSize = normalizePageSize(pageSize);
  const pageCount = Math.max(1, Math.ceil(safeTotal / safePageSize));
  const page = Math.min(Math.max(Math.floor(currentPage) || 1, 1), pageCount);
  const normalizedPagerCount = normalizePagerCount(pagerCount);
  const pageItems = useMemo(
    () => getPageItems(page, pageCount, normalizedPagerCount),
    [page, pageCount, normalizedPagerCount],
  );
  const [jumpPage, setJumpPage] = useState(String(page));
  const sizes = useMemo(() => {
    const values = pageSizes
      .filter((value) => Number.isFinite(value) && value > 0)
      .map((value) => Math.floor(value));
    return Array.from(new Set(values.includes(safePageSize) ? values : [...values, safePageSize]));
  }, [pageSizes, safePageSize]);
  const layoutItems = useMemo(() => parseLayout(layout), [layout]);

  useEffect(() => {
    setJumpPage(String(page));
  }, [page]);

  if (hideOnSinglePage && pageCount <= 1) return null;

  function changePage(nextPage: number) {
    const next = Math.min(Math.max(Math.floor(nextPage), 1), pageCount);
    if (!disabled && next !== page) onCurrentChange(next);
  }

  function submitJump() {
    const next = Number.parseInt(jumpPage, 10);
    if (!Number.isFinite(next)) {
      setJumpPage(String(page));
      return;
    }
    const clamped = Math.min(Math.max(next, 1), pageCount);
    setJumpPage(String(clamped));
    changePage(clamped);
  }

  function renderItem(item: LayoutItem | "->", index: number) {
    if (item === "->") return <span key={`spacer-${index}`} className="common-pagination-spacer" aria-hidden="true" />;

    switch (item) {
      case "total":
        return <span key={item} className="common-pagination-total">共 {formatNumber(safeTotal)} 条</span>;
      case "sizes":
        return (
          <div key={item} className="common-pagination-sizes">
            <span className="common-pagination-label">每页</span>
            <Select
              value={String(safePageSize)}
              ariaLabel="每页条数"
              disabled={disabled || !onSizeChange}
              onChange={(value) => onSizeChange?.(Number(value))}
              options={sizes.map((size) => ({ value: String(size), label: `${size} 条` }))}
              className="common-pagination-size-select"
            />
          </div>
        );
      case "prev":
        return (
          <Button
            variant="unstyled"
            key={item}
            type="button"
            className="common-pagination-button"
            aria-label="上一页"
            title="上一页"
            disabled={disabled || page <= 1}
            onClick={() => changePage(page - 1)}
          >
            <ChevronLeft size={16} aria-hidden="true" />
          </Button>
        );
      case "pager":
        return (
          <div key={item} className="common-pagination-pager" role="list" aria-label="页码">
            {pageItems.map((pageItem, pageIndex) => {
              if (typeof pageItem === "number") {
                const active = pageItem === page;
                return (
                  <Button
                    variant="unstyled"
                    key={pageItem}
                    type="button"
                    className={`common-pagination-page${active ? " is-active" : ""}`}
                    aria-current={active ? "page" : undefined}
                    aria-label={`第 ${pageItem} 页`}
                    role="listitem"
                    disabled={disabled || active}
                    onClick={() => changePage(pageItem)}
                  >
                    {pageItem}
                  </Button>
                );
              }
              const forward = pageItem === "next-more";
              const target = forward ? page + normalizedPagerCount : page - normalizedPagerCount;
              return (
                <Button
                  variant="unstyled"
                  key={`${pageItem}-${pageIndex}`}
                  type="button"
                  className="common-pagination-more"
                  aria-label={forward ? "跳到后面的页码" : "跳到前面的页码"}
                  title={forward ? "跳到后面的页码" : "跳到前面的页码"}
                  disabled={disabled}
                  onClick={() => changePage(target)}
                >
                  <MoreHorizontal size={16} aria-hidden="true" />
                </Button>
              );
            })}
          </div>
        );
      case "next":
        return (
          <Button
            variant="unstyled"
            key={item}
            type="button"
            className="common-pagination-button"
            aria-label="下一页"
            title="下一页"
            disabled={disabled || page >= pageCount}
            onClick={() => changePage(page + 1)}
          >
            <ChevronRight size={16} aria-hidden="true" />
          </Button>
        );
      case "jumper":
        return (
          <label key={item} className="common-pagination-jumper">
            <span>前往</span>
            <input
              aria-label="跳转页码"
              type="number"
              min={1}
              max={pageCount}
              value={jumpPage}
              disabled={disabled}
              onChange={(event) => setJumpPage(event.target.value)}
              onKeyDown={(event) => { if (event.key === "Enter") submitJump(); }}
            />
            <span>页</span>
            <Button variant="unstyled" className="common-pagination-jump-button" disabled={disabled} onClick={submitJump}>确定</Button>
          </label>
        );
    }
  }

  return (
    <nav
      className={`log-pagination common-pagination${background ? " common-pagination--background" : ""}${small ? " common-pagination--small" : ""} ${className}`.trim()}
      aria-label={ariaLabel}
    >
      {layoutItems.map(renderItem)}
    </nav>
  );
}
