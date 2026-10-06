import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { Check, Minus, Trash2 } from "lucide-react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { Button } from "../../app/controls/Button";
import { DataTable, TableState } from "../../app/data/DataTable";
import { Pagination } from "../../app/data/Pagination";
import { DEFAULT_PAGE_SIZE, PAGE_SIZE_OPTIONS } from "../../app/data/pagination.constants";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import {
  batchDeleteAccounts,
  deleteAccount,
  fetchAccounts,
  refreshAccount,
  setAccountEnabled,
  type AccountFilters,
  type AccountRecord,
} from "./accountsApi";
import { Select } from "../../app/controls/Select";
import { AccountOnboardingDialog } from "./AccountOnboardingDialog";
import { refreshModels } from "../models/modelsApi";

type FilterDraft = { channel: string; status: string; search: string };
const emptyFilters: FilterDraft = { channel: "", status: "", search: "" };

type PendingDelete =
  | { kind: "single"; accountId: string; accountName: string }
  | { kind: "batch"; accountIds: string[] };

const statusLabels: Record<string, string> = {
  ready: "就绪",
  busy: "处理中",
  cooldown: "冷却",
  limited: "受限",
  needLogin: "需登录",
  captcha: "需验证",
  disabled: "已停用",
  expired: "已过期",
  error: "异常",
  no_entitlement: "无模型权益",
  unknown: "未知",
};

const runtimeStateLabels: Record<string, string> = {
  closed: "正常",
  cooldown: "冷却",
  breaker_open: "熔断",
};

function statusTone(status: string): string {
  if (status === "ready") return "status-success";
  if (status === "busy") return "status-info";
  if (status === "cooldown" || status === "limited" || status === "captcha") return "status-warning";
  if (status === "needLogin" || status === "disabled" || status === "expired" || status === "error" || status === "no_entitlement") return "status-danger";
  return "status-neutral";
}

function displayName(name: string): string {
  const [local, domain, extra] = name.split("@");
  if (!domain || extra !== undefined || local.length < 2) return name;
  return `${local.slice(0, 1)}•••@${domain}`;
}

function formatTimestamp(value: number | string | null | undefined): string {
  if (value === null || value === undefined || value === "") return "—";
  const numeric = typeof value === "number"
    ? value
    : /^-?\d+(?:\.\d+)?$/.test(value.trim()) ? Number(value) : NaN;
  const timestamp = Number.isFinite(numeric)
    ? new Date(Math.abs(numeric) < 1_000_000_000_000 ? numeric * 1000 : numeric)
    : new Date(String(value));
  if (numeric === 0) return "永不过期";
  if (Number.isNaN(timestamp.getTime())) return "—";
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "short",
    timeZone: "UTC",
  }).format(timestamp);
}

function quotaLabel(account: AccountRecord): string {
  if (account.quota_unit === "credits_remaining") {
    return `${new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 }).format(account.quota_total)} credits 剩余`;
  }
  return "不适用";
}

function channelDisplayName(slug: string, channels: ChannelOverview[]): string {
  return channels.find((channel) => channel.slug === slug)?.name ?? slug;
}

export function AccountsPage() {
  const { role } = useOutletContext<{ role: "admin" | "viewer" }>();
  const canManageAccounts = role === "admin";
  const [rows, setRows] = useState<AccountRecord[]>([]);
  const [channels, setChannels] = useState<ChannelOverview[]>([]);
  const [channelsLoading, setChannelsLoading] = useState(true);
  const [channelsError, setChannelsError] = useState("");
  const [channelsRetry, setChannelsRetry] = useState(0);
  const [unconfiguredChannels, setUnconfiguredChannels] = useState<string[]>([]);
  const [draft, setDraft] = useState<FilterDraft>(emptyFilters);
  const [filters, setFilters] = useState<AccountFilters>({});
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [noticeMessage, setNoticeMessage] = useState("");
  const [onboardingOpen, setOnboardingOpen] = useState(false);
  const [accountActionId, setAccountActionId] = useState<string | null>(null);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [batchBusy, setBatchBusy] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<PendingDelete | null>(null);
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setChannelsLoading(true);
    setChannelsError("");
    fetchChannels(controller.signal)
      .then(setChannels)
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setChannels([]);
          setChannelsError(cause instanceof ApiClientError ? cause.message : "读取渠道配置失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setChannelsLoading(false);
      });
    return () => controller.abort();
  }, [channelsRetry]);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    setSelectedIds([]);
    fetchAccounts(page, filters, controller.signal, pageSize)
      .then((result) => {
        setRows(result.data);
        setTotal(result.pagination.total);
        setUnconfiguredChannels(result.unconfigured_channels ?? []);
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof ApiClientError ? cause.message : "读取本地账号快照失败");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [page, pageSize, filters, retry]);

  function applyDraftFilters(nextDraft: FilterDraft) {
    const next: AccountFilters = {};
    if (nextDraft.channel) next.channel = nextDraft.channel;
    if (nextDraft.status) next.status = nextDraft.status;
    if (nextDraft.search.trim()) next.search = nextDraft.search.trim();
    setDraft(nextDraft);
    setPage(1);
    setFilters(next);
  }

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    applyDraftFilters(draft);
  }

  function clearFilters() {
    setDraft(emptyFilters);
    setFilters({});
    setPage(1);
  }

  const handleOnboardingComplete = useCallback((message: string, channel?: string) => {
    setOnboardingOpen(false);
    setNoticeMessage(message);
    setPage(1);
    setRetry((value) => value + 1);
    if (channel) void refreshModels(channel).catch(() => undefined);
  }, []);

  async function handleAccountEnabled(account: AccountRecord) {
    setAccountActionId(account.id);
    setError("");
    try {
      await setAccountEnabled(account.id, !account.enabled);
      setNoticeMessage(account.enabled ? "账号已停用" : "账号已启用");
      setRetry((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "更新账号状态失败");
    } finally {
      setAccountActionId(null);
    }
  }

  function requestAccountDelete(account: AccountRecord) {
    if (accountActionId || batchBusy) return;
    setPendingDelete({
      kind: "single",
      accountId: account.id,
      accountName: displayName(account.name),
    });
  }

  async function handleAccountRefresh(account: AccountRecord) {
    setAccountActionId(account.id);
    setError("");
    try {
      await refreshAccount(account.id);
      setNoticeMessage("账号凭据已刷新");
      setRetry((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "刷新账号凭据失败");
    } finally {
      setAccountActionId(null);
    }
  }

  function toggleSelected(accountId: string, selected: boolean) {
    setSelectedIds((current) =>
      selected
        ? Array.from(new Set([...current, accountId]))
        : current.filter((id) => id !== accountId),
    );
  }

  const selectableIds = canManageAccounts ? rows.map((row) => row.id) : [];
  const allSelected = selectableIds.length > 0 && selectableIds.every((id) => selectedIds.includes(id));
  const someSelected = selectableIds.some((id) => selectedIds.includes(id));

  function toggleSelectAll(next: boolean) {
    setSelectedIds(next ? selectableIds : []);
  }

  function requestBatchDelete() {
    if (selectedIds.length === 0 || batchBusy) return;
    setPendingDelete({ kind: "batch", accountIds: [...selectedIds] });
  }

  async function confirmDelete() {
    if (!pendingDelete) return;
    const action = pendingDelete;
    setPendingDelete(null);
    setError("");

    if (action.kind === "batch") {
      setBatchBusy(true);
      try {
        const result = await batchDeleteAccounts(action.accountIds);
        const failedCount = result.failed.length;
        setNoticeMessage(
          failedCount
            ? `已删除 ${result.deleted.length} 个账号，${failedCount} 个删除失败`
            : `已删除 ${result.deleted.length} 个账号及其凭据`,
        );
        setSelectedIds([]);
        setRetry((value) => value + 1);
      } catch (cause: unknown) {
        setError(cause instanceof ApiClientError ? cause.message : "批量删除账号失败");
      } finally {
        setBatchBusy(false);
      }
      return;
    }

    setAccountActionId(action.accountId);
    try {
      await deleteAccount(action.accountId);
      setNoticeMessage("账号及其凭据已删除");
      setRetry((value) => value + 1);
    } catch (cause: unknown) {
      setError(cause instanceof ApiClientError ? cause.message : "删除账号失败");
    } finally {
      setAccountActionId(null);
    }
  }

  return (
    <main className="page-content data-page accounts-page">
      <div className="page-heading">
        <div>
          <span className="page-eyebrow">ACCOUNT POOL</span>
          <h1>账号池</h1>
          <p>{total} 个本地账号</p>
        </div>
        {canManageAccounts && (
          <div className="page-actions">
            <Button variant="secondary" onClick={() => setOnboardingOpen(true)}>新增账号</Button>
            <Button
              variant="danger"
              onClick={requestBatchDelete}
              disabled={batchBusy || selectedIds.length === 0}
              title={selectedIds.length === 0 ? "请先选择账号" : `删除已选择的 ${selectedIds.length} 个账号`}
            >
              <Trash2 size={15} aria-hidden="true" />
              {batchBusy ? "删除中…" : "批量删除"}
            </Button>
          </div>
        )}
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <Button variant="secondary" size="sm" onClick={() => setRetry((value) => value + 1)}>重试</Button>
        </div>
      )}
      {channelsError && (
        <div className="notice notice-error" role="alert">
          <span>{channelsError}。新增账号和渠道筛选暂时不可用。</span>
          <Button variant="secondary" size="sm" onClick={() => setChannelsRetry((value) => value + 1)}>重试</Button>
        </div>
      )}
      {noticeMessage && <div className="notice notice-info" role="status">{noticeMessage}</div>}
      {unconfiguredChannels.length > 0 && (
        <div className="notice notice-warning account-configuration-notice" role="status">
          <span>
            部分渠道暂不能新增账号：未配置账号管理接口（{unconfiguredChannels.map((slug) => channelDisplayName(slug, channels)).join("、")}）。
            请在服务端补齐对应的上游地址和管理凭据后重试。
          </span>
        </div>
      )}

      <form className="log-filter-form account-filter-form" onSubmit={applyFilters}>
        <label><span>渠道</span><Select
          value={draft.channel}
          onChange={(channel) => applyDraftFilters({ ...draft, channel })}
          options={[
            { value: "", label: "全部渠道" },
            ...channels.map((channel) => ({ value: channel.slug, label: channel.name })),
          ]}
        /></label>
        <label><span>状态</span><Select
          value={draft.status}
          onChange={(status) => applyDraftFilters({ ...draft, status })}
          options={[
            { value: "", label: "全部状态" },
            ...Object.entries(statusLabels).map(([value, label]) => ({ value, label })),
          ]}
        /></label>
        <label><span>名称</span><input value={draft.search} onChange={(event) => setDraft({ ...draft, search: event.target.value })} placeholder="搜索账号名称" /></label>
        <div className="log-filter-actions">
          <Button type="submit" variant="primary">筛选</Button>
          <Button variant="secondary" onClick={clearFilters}>清除</Button>
        </div>
      </form>

      {loading ? (
        <TableState live>正在读取本地账号快照…</TableState>
      ) : rows.length === 0 ? (
        <TableState>
          <span>没有符合条件的账号</span>
          {canManageAccounts && <span className="secondary-text">完成渠道授权后，账号会自动出现在本地账号池。</span>}
        </TableState>
      ) : (
        <DataTable className={`account-table ${canManageAccounts ? "has-row-actions" : ""}`.trim()} ariaLabel="账号列表">
            <thead><tr>
              {canManageAccounts && (
                <th className="account-select-col">
                  <SelectCheckbox
                    ariaLabel="全选本页账号"
                    checked={allSelected}
                    indeterminate={someSelected && !allSelected}
                    disabled={batchBusy}
                    onChange={toggleSelectAll}
                  />
                </th>
              )}
              <th>账号</th><th>渠道</th><th>状态</th><th>额度说明</th><th>到期</th><th>快照时间</th>{canManageAccounts && <th>操作</th>}
            </tr></thead>
            <tbody>{rows.map((account) => (
              <AccountRow
                key={account.id}
                account={account}
                canManage={canManageAccounts}
                busy={accountActionId === account.id || batchBusy}
                selected={selectedIds.includes(account.id)}
                onSelect={(next) => toggleSelected(account.id, next)}
                onToggle={() => void handleAccountEnabled(account)}
                onRefresh={() => void handleAccountRefresh(account)}
                onDelete={() => requestAccountDelete(account)}
              />
            ))}</tbody>
        </DataTable>
      )}

      <Pagination
        currentPage={page}
        pageSize={pageSize}
        total={total}
        pageSizes={PAGE_SIZE_OPTIONS}
        disabled={loading}
        onCurrentChange={setPage}
        onSizeChange={(nextPageSize) => { setPage(1); setPageSize(nextPageSize); }}
      />
      {pendingDelete && (
        <div
          className="account-delete-backdrop"
          role="presentation"
          onMouseDown={(event) => {
            if (event.target === event.currentTarget && !batchBusy && !accountActionId) setPendingDelete(null);
          }}
        >
          <section className="account-delete-dialog" role="alertdialog" aria-modal="true" aria-labelledby="account-delete-title">
            <div className="account-delete-heading">
              <div>
                <span className="page-eyebrow">危险操作</span>
                <h2 id="account-delete-title">确认删除账号</h2>
              </div>
            </div>
            <p>
              {pendingDelete.kind === "single"
                ? `确定删除账号“${pendingDelete.accountName}”吗？`
                : `确定删除已选择的 ${pendingDelete.accountIds.length} 个账号吗？`}
            </p>
            <p className="secondary-text">加密凭据和本地 profile 会一并销毁，此操作不可撤销。</p>
            <div className="account-delete-actions">
              <Button variant="secondary" onClick={() => setPendingDelete(null)} disabled={batchBusy || Boolean(accountActionId)}>取消</Button>
              <Button variant="danger" onClick={() => void confirmDelete()} disabled={batchBusy || Boolean(accountActionId)}>
                <Trash2 size={15} aria-hidden="true" />确认删除
              </Button>
            </div>
          </section>
        </div>
      )}
      <AccountOnboardingDialog
        open={onboardingOpen}
        channels={channels}
        channelsLoading={channelsLoading}
        channelsError={channelsError}
        onRetryChannels={() => setChannelsRetry((value) => value + 1)}
        onClose={() => setOnboardingOpen(false)}
        onComplete={handleOnboardingComplete}
      />
    </main>
  );
}

function SelectCheckbox({
  ariaLabel,
  checked,
  indeterminate = false,
  disabled = false,
  onChange,
}: {
  ariaLabel: string;
  checked: boolean;
  indeterminate?: boolean;
  disabled?: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <button
      type="button"
      className="control-checkbox is-bare account-select-control"
      role="checkbox"
      aria-checked={indeterminate ? "mixed" : checked}
      aria-label={ariaLabel}
      disabled={disabled}
      onClick={() => onChange(indeterminate ? true : !checked)}
    >
      <span className="control-checkbox-box" aria-hidden="true">
        {indeterminate ? <Minus size={13} strokeWidth={3} /> : checked ? <Check size={13} strokeWidth={3} /> : null}
      </span>
    </button>
  );
}

function AccountRow({
  account,
  canManage,
  busy,
  selected,
  onSelect,
  onToggle,
  onRefresh,
  onDelete,
}: {
  account: AccountRecord;
  canManage: boolean;
  busy: boolean;
  selected: boolean;
  onSelect: (checked: boolean) => void;
  onToggle: () => void;
  onRefresh: () => void;
  onDelete: () => void;
}) {
  const status = statusLabels[account.status] ?? account.status;
  const tone = statusTone(account.status);
  const runtime = account.gateway_runtime;
  const runtimeLabel = runtimeStateLabels[runtime.state];
  const runtimeUntil = runtime.state === "breaker_open"
    ? runtime.breaker_until
    : runtime.state === "cooldown" ? runtime.cooldown_until : null;
  return (
    <tr className={selected ? "is-selected" : undefined}>
      {canManage && (
        <td className="account-select-col">
          <SelectCheckbox
            ariaLabel={`选择账号 ${displayName(account.name)}`}
            checked={selected}
            disabled={busy}
            onChange={onSelect}
          />
        </td>
      )}
      <td><span className="channel-name">{displayName(account.name)}</span><span className="secondary-text">{account.kind}{account.tier ? ` · ${account.tier}` : ""}</span></td>
      <td>{account.channel}</td>
      <td>
        <span className={`status-label ${tone}`}>{status}</span>
        {runtimeLabel && <span className="secondary-text">
          网关观察：{runtimeLabel} · 成功 {runtime.success_count} / 失败 {runtime.fail_count}
          {runtimeUntil ? ` · 截止 ${formatTimestamp(runtimeUntil)}` : ""}
        </span>}
      </td>
      <td>{quotaLabel(account)}</td>
      <td>{formatTimestamp(account.expires_at)}</td>
      <td>{formatTimestamp(account.updated_at)}</td>
      {canManage && <td className="account-actions">
        <Button variant="secondary" size="sm" className="compact-action" onClick={onToggle} disabled={busy}>
          {busy ? "处理中…" : account.enabled ? "停用" : "启用"}
        </Button>
        <Button variant="secondary" size="sm" className="compact-action" onClick={onRefresh} disabled={busy || !account.enabled}>
          刷新凭据
        </Button>
        <Button variant="danger" size="sm" className="compact-action" onClick={onDelete} disabled={busy}>
          删除
        </Button>
      </td>}
    </tr>
  );
}
