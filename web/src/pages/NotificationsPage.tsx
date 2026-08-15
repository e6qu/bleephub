import { useState } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import {
  deleteThreadSubscription,
  fetchNotifications,
  getThreadSubscription,
  markAllNotificationsRead,
  markThreadDone,
  markThreadRead,
  setThreadSubscription,
  isForbidden,
  isRateLimited,
  ghSend,
} from "../api.js";

const enc = encodeURIComponent;
import type { GithubNotificationThread, GithubThreadSubscription } from "../types.js";
import {
  Box,
  Button,
  DialogActions,
  ErrorBanner,
  Modal,
  PageTitle,
  StateLabel,
  Tabs,
} from "../components/ui.js";
import { NotificationBellIcon } from "../components/octicons.js";

const col = createColumnHelper<GithubNotificationThread>();

export function NotificationsPage() {
  const [tab, setTab] = useState<"unread" | "all">("unread");

  return (
    <div>
      <PageTitle
        icon={<NotificationBellIcon size={20} />}
        title="Notifications"
        meta="Issue and pull request activity across repositories you can access."
      />

      <Tabs<"unread" | "all">
        items={[
          { key: "unread", label: "Unread" },
          { key: "all", label: "All" },
        ]}
        active={tab}
        onChange={setTab}
      />

      <ThreadsTable all={tab === "all"} />
    </div>
  );
}

function ThreadsTable({ all }: { all: boolean }) {
  const queryClient = useQueryClient();
  const [mutationError, setMutationError] = useState<string | null>(null);
  const [activeThread, setActiveThread] = useState<GithubNotificationThread | null>(null);
  // github.com's inbox filters by repository and by reason; both narrow the
  // table below the Unread/All tabs and the free-text filter.
  const [repoFilter, setRepoFilter] = useState("");
  const [reasonFilter, setReasonFilter] = useState("");
  // github.com's inbox groups threads under per-repository headers; offer that
  // as an alternative to the flat table.
  const [groupByRepo, setGroupByRepo] = useState(false);

  const { data, isLoading, isError } = useQuery({
    queryKey: ["notifications", all],
    queryFn: () => fetchNotifications({ all }),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 10000,
  });

  const readMut = useMutation({
    mutationFn: (id: string) => markThreadRead(id),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["notifications"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });
  const markAllMut = useMutation({
    mutationFn: markAllNotificationsRead,
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["notifications"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });
  const doneMut = useMutation({
    mutationFn: (id: string) => markThreadDone(id),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["notifications"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });
  // github.com clears a whole repository's notifications from its group header:
  // PUT /repos/{owner}/{repo}/notifications marks them all read.
  const markRepoMut = useMutation({
    mutationFn: (fullName: string) => {
      const [owner = "", repo = ""] = fullName.split("/");
      return ghSend("PUT", `/api/v3/repos/${enc(owner)}/${enc(repo)}/notifications`);
    },
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["notifications"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });

  if (isError) return <InlineError title="Failed to load notifications" />;
  if (isLoading || !data) return <Spinner label="loading notifications" />;

  const repoName = (t: GithubNotificationThread) =>
    typeof t.repository.full_name === "string" ? t.repository.full_name : "";
  const repoOptions = [...new Set(data.map(repoName).filter(Boolean))].sort();
  const reasonOptions = [...new Set(data.map((t) => t.reason).filter(Boolean))].sort();
  const filtered = data
    .filter((t) => all || t.unread)
    .filter((t) => !repoFilter || repoName(t) === repoFilter)
    .filter((t) => !reasonFilter || t.reason === reasonFilter);

  const columns = [
    col.accessor("unread", {
      header: "Status",
      cell: (info) =>
        info.getValue() ? (
          <StateLabel state="open">unread</StateLabel>
        ) : (
          <StateLabel state="closed">read</StateLabel>
        ),
    }),
    col.accessor("subject", {
      header: "Subject",
      cell: (info) => {
        const subject = info.getValue();
        const href = subjectUrlToUI(subject.url);
        return href ? (
          <Link to={href} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
            {subject.title}
          </Link>
        ) : (
          <span>{subject.title}</span>
        );
      },
    }),
    col.accessor("subject", {
      header: "Type",
      id: "type",
      cell: (info) => (
        <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
          {info.getValue().type}
        </span>
      ),
    }),
    col.accessor("reason", {
      header: "Reason",
      cell: (info) => (
        <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
          {info.getValue()}
        </span>
      ),
    }),
    col.accessor("repository", {
      header: "Repository",
      cell: (info) => {
        const repo = info.getValue();
        const fullName = typeof repo.full_name === "string" ? repo.full_name : "";
        return (
          <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>{fullName}</span>
        );
      },
    }),
    col.accessor("updated_at", {
      header: "Updated",
      cell: (info) => new Date(info.getValue()).toLocaleString(),
    }),
    col.display({
      id: "actions",
      header: "Actions",
      cell: (info) => {
        const thread = info.row.original;
        return (
          <div className="flex flex-wrap items-center gap-1">
            {thread.unread && (
              <Button
                size="sm"
                variant="secondary"
                onClick={() => readMut.mutate(thread.id)}
                disabled={readMut.isPending}
              >
                Mark read
              </Button>
            )}
            <Button size="sm" variant="ghost" onClick={() => setActiveThread(thread)}>
              Subscription
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => doneMut.mutate(thread.id)}
              disabled={doneMut.isPending}
            >
              Done
            </Button>
          </div>
        );
      },
    }),
  ];

  const filterSelectStyle = { fontSize: "0.82rem", padding: "0.2rem 0.4rem" };
  return (
    <>
      {mutationError && <ErrorBanner>{mutationError}</ErrorBanner>}
      <div className="mb-3 flex flex-wrap items-end justify-between gap-2">
        <div className="flex flex-wrap items-end gap-2">
          <label className="flex flex-col gap-1" style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
            Repository
            <select
              aria-label="Filter by repository"
              value={repoFilter}
              onChange={(e) => setRepoFilter(e.target.value)}
              style={filterSelectStyle}
            >
              <option value="">All repositories</option>
              {repoOptions.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </label>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
            Reason
            <select
              aria-label="Filter by reason"
              value={reasonFilter}
              onChange={(e) => setReasonFilter(e.target.value)}
              style={filterSelectStyle}
            >
              <option value="">All reasons</option>
              {reasonOptions.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </label>
        </div>
        <div className="flex items-end gap-2">
          <div role="group" aria-label="Group notifications" className="flex gap-1">
            {(["list", "repo"] as const).map((mode) => (
              <Button
                key={mode}
                size="sm"
                variant={(mode === "repo") === groupByRepo ? "primary" : "secondary"}
                aria-pressed={(mode === "repo") === groupByRepo}
                onClick={() => setGroupByRepo(mode === "repo")}
              >
                {mode === "list" ? "List" : "By repository"}
              </Button>
            ))}
          </div>
          {!all && filtered.length > 0 && (
            <Button
              size="sm"
              variant="secondary"
              onClick={() => markAllMut.mutate()}
              disabled={markAllMut.isPending}
            >
              {markAllMut.isPending ? "Marking…" : "Mark all as read"}
            </Button>
          )}
        </div>
      </div>
      {groupByRepo ? (
        <NotificationsByRepo
          threads={filtered}
          repoName={repoName}
          onRead={(id) => readMut.mutate(id)}
          onDone={(id) => doneMut.mutate(id)}
          onSubscription={setActiveThread}
          onMarkRepoRead={(fullName) => markRepoMut.mutate(fullName)}
          repoBusy={markRepoMut.isPending}
          busy={readMut.isPending || doneMut.isPending}
          emptyMessage={all ? "No notifications." : "No unread notifications."}
        />
      ) : (
        <DataTable
          data={filtered ?? []}
          columns={columns}
          filterPlaceholder="Filter notifications…"
          emptyMessage={all ? "No notifications." : "No unread notifications."}
        />
      )}
      {activeThread && (
        <SubscriptionDialog thread={activeThread} onClose={() => setActiveThread(null)} />
      )}
    </>
  );
}

function NotificationsByRepo({
  threads,
  repoName,
  onRead,
  onDone,
  onSubscription,
  onMarkRepoRead,
  repoBusy,
  busy,
  emptyMessage,
}: {
  threads: GithubNotificationThread[];
  repoName: (t: GithubNotificationThread) => string;
  onRead: (id: string) => void;
  onDone: (id: string) => void;
  onSubscription: (t: GithubNotificationThread) => void;
  onMarkRepoRead: (fullName: string) => void;
  repoBusy: boolean;
  busy: boolean;
  emptyMessage: string;
}) {
  if (threads.length === 0) {
    return (
      <Box>
        <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>{emptyMessage}</div>
      </Box>
    );
  }
  const groups = new Map<string, GithubNotificationThread[]>();
  for (const t of threads) {
    const name = repoName(t) || "(unknown repository)";
    const list = groups.get(name) ?? [];
    list.push(t);
    groups.set(name, list);
  }
  return (
    <div className="flex flex-col gap-4">
      {[...groups.entries()]
        .sort((a, b) => a[0].localeCompare(b[0]))
        .map(([repo, list]) => (
          <Box
            key={repo}
            header={
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span style={{ fontWeight: 600 }}>
                  {repo}{" "}
                  <span style={{ color: "var(--color-fg-muted)", fontWeight: 400 }}>({list.length})</span>
                </span>
                {repo.includes("/") && (
                  <Button
                    size="sm"
                    variant="secondary"
                    aria-label={`Mark all as read in ${repo}`}
                    disabled={repoBusy}
                    onClick={() => onMarkRepoRead(repo)}
                  >
                    Mark all as read
                  </Button>
                )}
              </div>
            }
          >
            <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
              {list.map((thread, i) => {
                const href = subjectUrlToUI(thread.subject.url);
                return (
                  <li
                    key={thread.id}
                    className="flex flex-wrap items-center gap-2"
                    style={{ padding: "0.5rem 1rem", borderBottom: i < list.length - 1 ? "1px solid var(--color-border)" : "none" }}
                  >
                    {thread.unread && <StateLabel state="open">unread</StateLabel>}
                    <span className="min-w-0 flex-1">
                      {href ? (
                        <Link
                          to={href}
                          style={{ display: "inline-block", color: "var(--color-accent)", textDecoration: "none", lineHeight: "1.625rem" }}
                        >
                          {thread.subject.title}
                        </Link>
                      ) : (
                        <span>{thread.subject.title}</span>
                      )}
                      <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem", marginLeft: "0.5rem" }}>
                        {thread.subject.type} · {thread.reason}
                      </span>
                    </span>
                    {thread.unread && (
                      <Button size="sm" variant="secondary" disabled={busy} onClick={() => onRead(thread.id)}>
                        Mark read
                      </Button>
                    )}
                    <Button size="sm" variant="ghost" onClick={() => onSubscription(thread)}>
                      Subscription
                    </Button>
                    <Button size="sm" variant="ghost" disabled={busy} onClick={() => onDone(thread.id)}>
                      Done
                    </Button>
                  </li>
                );
              })}
            </ul>
          </Box>
        ))}
    </div>
  );
}

function subjectUrlToUI(url: string): string | null {
  const m = url.match(/\/api\/v3\/repos\/([^/]+)\/([^/]+)\/(issues|pulls)\/(\d+)$/);
  if (!m) return null;
  const [, owner, repo, kind, number] = m;
  const path = kind === "pulls" ? "pulls" : "issues";
  return `/ui/repos/${owner}/${repo}/${path}/${number}`;
}

function SubscriptionDialog({
  thread,
  onClose,
}: {
  thread: GithubNotificationThread;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [mutationError, setMutationError] = useState<string | null>(null);

  const { data, isLoading, isError } = useQuery({
    queryKey: ["notifications", thread.id, "subscription"],
    queryFn: () => getThreadSubscription(thread.id),
  });

  const setMut = useMutation({
    mutationFn: async (subscribed: boolean) => {
      if (subscribed) {
        await setThreadSubscription(thread.id, true);
      } else {
        await deleteThreadSubscription(thread.id);
      }
    },
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["notifications", thread.id, "subscription"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });

  const subscribe = () => setMut.mutate(true);
  const unsubscribe = () => setMut.mutate(false);

  return (
    <Modal title="Thread subscription" onClose={onClose}>
      <Box header={thread.subject.title} className="mb-4">
        <div style={{ padding: "1rem" }}>
          {isLoading ? (
            <Spinner label="loading subscription" />
          ) : isError ? (
            <InlineError title="Failed to load subscription" />
          ) : (
            <SubscriptionState subscription={data ?? null} />
          )}
        </div>
      </Box>

      {mutationError && <ErrorBanner>{mutationError}</ErrorBanner>}

      <DialogActions>
        <Button onClick={onClose} variant="ghost">
          Close
        </Button>
        <Button onClick={subscribe} disabled={setMut.isPending} variant="secondary">
          Subscribe
        </Button>
        <Button onClick={unsubscribe} disabled={setMut.isPending} variant="danger">
          Unsubscribe
        </Button>
      </DialogActions>
    </Modal>
  );
}

function SubscriptionState({ subscription }: { subscription: GithubThreadSubscription | null }) {
  if (!subscription) {
    return (
      <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
        No explicit subscription set for this thread.
      </div>
    );
  }
  return (
    <div style={{ fontSize: "0.85rem" }}>
      <div>
        <strong>Subscribed:</strong> {subscription.subscribed ? "yes" : "no"}
      </div>
      <div>
        <strong>Ignored:</strong> {subscription.ignored ? "yes" : "no"}
      </div>
      <div style={{ color: "var(--color-fg-muted)" }}>Reason: {subscription.reason}</div>
    </div>
  );
}
