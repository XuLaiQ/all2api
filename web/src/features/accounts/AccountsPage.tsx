import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useOutletContext } from "react-router-dom";
import { ApiClientError } from "../../api/client";
import { fetchChannels, type ChannelOverview } from "../usage/usageApi";
import {
  deleteAccount,
  fetchAccounts,
  setAccountEnabled,
  type AccountFilters,
  type AccountRecord,
} from "./accountsApi";
import { Select } from "../../app/controls/Select";
import { AccountOnboardingDialog } from "./AccountOnboardingDialog";

type FilterDraft = { channel: string; status: string; search: string };
const emptyFilters: FilterDraft = { channel: "", status: "", search: "" };

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
  if (status === "needLogin" || status === "disabled" || status === "expired" || status === "error") return "status-danger";
  return "status-neutral";
}

function displayName(name: string): string {
  const [local, domain, extra] = name.split("@");
  if (!domain || extra !== undefined || local.length < 2) return name;
  return `${local.slice(0, 1)}•••@${domain}`;
}

function formatTimestamp(value: number | null): string {
  if (!value) return "—";
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "short",
    timeZone: "UTC",
  }).format(new Date(value * 1000));
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
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [noticeMessage, setNoticeMessage] = useState("");
  const [onboardingOpen, setOnboardingOpen] = useState(false);
  const [accountActionId, setAccountActionId] = useState<string | null>(null);
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
    fetchAccounts(page, filters, controller.signal)
      .then((result) => {
        setRows(result.data);
        setTotal(result.pagination.total);
        setTotalPages(result.pagination.total_pages);
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
  }, [page, filters, retry]);

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next: AccountFilters = {};
    if (draft.channel) next.channel = draft.channel;
    if (draft.status) next.status = draft.status;
    if (draft.search.trim()) next.search = draft.search.trim();
    setPage(1);
    setFilters(next);
  }

  function clearFilters() {
    setDraft(emptyFilters);
    setFilters({});
    setPage(1);
  }

  const handleOnboardingComplete = useCallback((message: string) => {
    setOnboardingOpen(false);
    setNoticeMessage(message);
    setPage(1);
    setRetry((value) => value + 1);
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

  async function handleAccountDelete(account: AccountRecord) {
    if (!window.confirm(`确定删除账号“${displayName(account.name)}”吗？加密凭据和本地 profile 会一并销毁。`)) return;
    setAccountActionId(account.id);
    setError("");
    try {
      await deleteAccount(account.id);
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
            <button className="secondary-action-button" type="button" onClick={() => setOnboardingOpen(true)}>新增账号</button>
          </div>
        )}
      </div>

      {error && (
        <div className="notice notice-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRetry((value) => value + 1)}>重试</button>
        </div>
      )}
      {channelsError && (
        <div className="notice notice-error" role="alert">
          <span>{channelsError}。新增账号和渠道筛选暂时不可用。</span>
          <button type="button" onClick={() => setChannelsRetry((value) => value + 1)}>重试</button>
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
          onChange={(channel) => setDraft({ ...draft, channel })}
          options={[
            { value: "", label: "全部渠道" },
            ...channels.map((channel) => ({ value: channel.slug, label: channel.name })),
          ]}
        /></label>
        <label><span>状态</span><Select
          value={draft.status}
          onChange={(status) => setDraft({ ...draft, status })}
          options={[
            { value: "", label: "全部状态" },
            ...Object.entries(statusLabels).map(([value, label]) => ({ value, label })),
          ]}
        /></label>
        <label><span>名称</span><input value={draft.search} onChange={(event) => setDraft({ ...draft, search: event.target.value })} placeholder="搜索账号名称" /></label>
        <div className="log-filter-actions">
          <button type="submit">筛选</button>
          <button type="button" className="secondary-action" onClick={clearFilters}>清除</button>
        </div>
      </form>

      {loading ? (
        <div className="table-state" aria-live="polite">正在读取本地账号快照…</div>
      ) : rows.length === 0 ? (
        <div className="table-state">
          <span>没有符合条件的账号</span>
          {canManageAccounts && <span className="secondary-text">完成渠道授权后，账号会自动出现在本地账号池。</span>}
        </div>
      ) : (
        <div className="table-wrap">
          <table className="data-table account-table">
            <thead><tr><th>账号</th><th>渠道</th><th>状态</th><th>额度说明</th><th>到期</th><th>快照时间</th>{canManageAccounts && <th>操作</th>}</tr></thead>
            <tbody>{rows.map((account) => (
              <AccountRow
                key={account.id}
                account={account}
                canManage={canManageAccounts}
                busy={accountActionId === account.id}
                onToggle={() => void handleAccountEnabled(account)}
                onDelete={() => void handleAccountDelete(account)}
              />
            ))}</tbody>
          </table>
        </div>
      )}

      <div className="log-pagination">
        <span>第 {page} / {Math.max(totalPages, 1)} 页</span>
        <div>
          <button type="button" disabled={loading || page <= 1} onClick={() => setPage((value) => value - 1)}>上一页</button>
          <button type="button" disabled={loading || page >= totalPages} onClick={() => setPage((value) => value + 1)}>下一页</button>
        </div>
      </div>
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

function AccountRow({
  account,
  canManage,
  busy,
  onToggle,
  onDelete,
}: {
  account: AccountRecord;
  canManage: boolean;
  busy: boolean;
  onToggle: () => void;
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
    <tr>
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
        <button type="button" className="secondary-action compact-action" onClick={onToggle} disabled={busy}>
          {busy ? "处理中…" : account.enabled ? "停用" : "启用"}
        </button>
        <button type="button" className="danger-action compact-action" onClick={onDelete} disabled={busy}>
          删除
        </button>
      </td>}
    </tr>
  );
}
