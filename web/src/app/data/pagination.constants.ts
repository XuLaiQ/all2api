export type PaginationMeta = {
  page: number;
  page_size: number;
  total: number;
  total_pages: number;
};

export const DEFAULT_PAGE_SIZE = 50;
export const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
