import { useState } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import {
  deleteThreadSubscription,
  fetchNotifications,
  getThreadSubscription,
  ghFetch,
  markAllNotificationsRead,
  markThreadDone,
  markThreadRead,
  setThreadSubscription,
  isForbidden,
  isRateLimited,
  ghSend,
} from "../api.js";

const enc = encodeURIComponent;
import type {
  GithubNotificationThread,
  GithubNotificationThreadWithSaved,
  GithubThreadSubscription,
} from "../types.js";
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
import { Avatar } from "../components/Avatar.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { NotificationBellIcon } from "../components/octicons.js";

const col = createColumnHelper<GithubNotificationThreadWithSaved>();

// Inbox is the REST notification list; Saved and Done are web-only views.
type NotificationView = "inbox" | "saved" | "done";

const GROUP_BY_REPO_KEY = "bleephub.notifications.group_by_repo";

// Saved/Done are served from /ui-data (REST thread shape plus a `saved` flag).
async function fetchNotificationView(view: "saved" | "done"): Promise<GithubNotificationThreadWithSaved[]> {
  const body = await ghFetch<unknown>(`/ui-data/notifications?view=${view}&per_page=100`);
  if (!Array.isArray(body)) throw new Error("malformed response: expected a JSON array");
  return body as GithubNotificationThreadWithSaved[];
}

export function NotificationsPage() {
  const [view, setView] = useState<NotificationView>("inbox");
  const [tab, setTab] = useState<"unread" | "all">("unread");

  return (
    <div>
      <PageTitle
        icon={<NotificationBellIcon size={20} />}
        title="Notifications"
        meta="Issue and pull request activity across repositories you can access."
      />

      <Tabs<NotificationView>
        items={[
          { key: "inbox", label: "Inbox" },
          { key: "saved", label: "Saved" },
          { key: "done", label: "Done" },
        ]}
        active={view}
        onChange={setView}
      />

      {view === "inbox" && (
        <div role="group" aria-label="Inbox scope" className="mb-3 flex gap-1">
          {(["unread", "all"] as const).map((scope) => (
            <Button
              key={scope}
              size="sm"
              variant={tab === scope ? "primary" : "secondary"}
              aria-pressed={tab === scope}
              onClick={() => setTab(scope)}
            >
              {scope === "unread" ? "Unread" : "All"}
            </Button>
          ))}
        </div>
      )}

      <ThreadsList view={view} all={tab === "all"} />
    </div>
  );
}

function ThreadsList({ view, all }: { view: NotificationView; all: boolean }) {
  const queryClient = useQueryClient();
  const [mutationError, setMutationError] = useState<string | null>(null);
  const [activeThread, setActiveThread] = useState<GithubNotificationThread | null>(null);
  const [repoFilter, setRepoFilter] = useState("");
  const [reasonFilter, setReasonFilter] = useState("");
  // Grouped by repository by default (github parity); the choice persists per browser.
  const [groupByRepo, setGroupByRepo] = useState(
    () => localStorage.getItem(GROUP_BY_REPO_KEY) !== "flat",
  );
  const setGrouping = (grouped: boolean) => {
    setGroupByRepo(grouped);
    localStorage.setItem(GROUP_BY_REPO_KEY, grouped ? "repo" : "flat");
  };

  const inboxQuery = useQuery({
    queryKey: ["notifications", all],
    queryFn: () => fetchNotifications({ all }),
    enabled: view === "inbox",
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 10000,
  });
  // Saved view doubles as the inbox's bookmark state; the REST list omits `saved`.
  const savedQuery = useQuery({
    queryKey: ["notifications", "view", "saved"],
    queryFn: () => fetchNotificationView("saved"),
  });
  const doneQuery = useQuery({
    queryKey: ["notifications", "view", "done"],
    queryFn: () => fetchNotificationView("done"),
    enabled: view === "done",
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["notifications"] });
  const mutationOptions = {
    onSuccess: () => {
      setMutationError(null);
      invalidate();
    },
    onError: (err: Error) => setMutationError(err.message),
  };
  const readMut = useMutation({ mutationFn: (id: string) => markThreadRead(id), ...mutationOptions });
  const markAllMut = useMutation({ mutationFn: markAllNotificationsRead, ...mutationOptions });
  const doneMut = useMutation({ mutationFn: (id: string) => markThreadDone(id), ...mutationOptions });
  const saveMut = useMutation({
    mutationFn: ({ id, saved }: { id: string; saved: boolean }) =>
      ghSend(saved ? "PUT" : "DELETE", `/ui-data/notifications/threads/${enc(id)}/saved`),
    ...mutationOptions,
  });
  // PUT /repos/{owner}/{repo}/notifications marks a whole repo's threads read.
  const markRepoMut = useMutation({
    mutationFn: (fullName: string) => {
      const [owner = "", repo = ""] = fullName.split("/");
      return ghSend("PUT", `/api/v3/repos/${enc(owner)}/${enc(repo)}/notifications`);
    },
    ...mutationOptions,
  });

  const activeQuery = view === "inbox" ? inboxQuery : view === "saved" ? savedQuery : doneQuery;
  if (activeQuery.isError) return <InlineError title="Failed to load notifications" />;
  if (activeQuery.isLoading || !activeQuery.data) return <Spinner label="loading notifications" />;

  const savedIds = new Set((savedQuery.data ?? []).map((t) => t.id));
  const withSaved = (t: GithubNotificationThread): GithubNotificationThreadWithSaved => ({
    ...t,
    saved: savedIds.has(t.id),
  });
  const data: GithubNotificationThreadWithSaved[] =
    view === "inbox"
      ? (inboxQuery.data ?? []).map(withSaved)
      : view === "saved"
        ? (savedQuery.data ?? [])
        : (doneQuery.data ?? []).map((t) => ({ ...t, saved: t.saved ?? savedIds.has(t.id) }));

  const repoName = (t: GithubNotificationThread) =>
    typeof t.repository.full_name === "string" ? t.repository.full_name : "";
  const repoOptions = [...new Set(data.map(repoName).filter(Boolean))].sort();
  const reasonOptions = [...new Set(data.map((t) => t.reason).filter(Boolean))].sort();
  const filtered = data
    .filter((t) => view !== "inbox" || all || t.unread)
    .filter((t) => !repoFilter || repoName(t) === repoFilter)
    .filter((t) => !reasonFilter || t.reason === reasonFilter);

  // Done has no mark-unread/undo endpoint, so its rows only link and toggle Saved.
  const readOnly = view === "done";
  const busy = readMut.isPending || doneMut.isPending || saveMut.isPending;

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
          <Link to={href} style={{ color: "var(--color-accent)", textDecoration: "none", overflowWrap: "anywhere" }}>
            {subject.title}
          </Link>
        ) : (
          <span style={{ overflowWrap: "anywhere" }}>{subject.title}</span>
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
          <span className="inline-flex items-center gap-1.5" style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
            <RepoAvatar repository={repo} size={18} />
            {fullName}
          </span>
        );
      },
    }),
    col.accessor("updated_at", {
      header: "Updated",
      cell: (info) => <RelativeTime iso={info.getValue()} />,
    }),
    col.display({
      id: "actions",
      header: "Actions",
      cell: (info) => {
        const thread = info.row.original;
        return (
          <div className="flex flex-wrap items-center gap-1">
            {!readOnly && thread.unread && (
              <Button
                size="sm"
                variant="secondary"
                onClick={() => readMut.mutate(thread.id)}
                disabled={readMut.isPending}
              >
                Mark read
              </Button>
            )}
            <SaveToggle thread={thread} disabled={saveMut.isPending} onToggle={(id, saved) => saveMut.mutate({ id, saved })} />
            {!readOnly && (
              <Button size="sm" variant="ghost" onClick={() => setActiveThread(thread)}>
                Subscription
              </Button>
            )}
            {!readOnly && (
              <Button
                size="sm"
                variant="ghost"
                onClick={() => doneMut.mutate(thread.id)}
                disabled={doneMut.isPending}
              >
                Done
              </Button>
            )}
          </div>
        );
      },
    }),
  ];

  const emptyMessage =
    view === "saved"
      ? "No saved notifications."
      : view === "done"
        ? "No notifications marked done."
        : all
          ? "No notifications."
          : "No unread notifications.";

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
                onClick={() => setGrouping(mode === "repo")}
              >
                {mode === "list" ? "List" : "By repository"}
              </Button>
            ))}
          </div>
          {view === "inbox" && !all && filtered.length > 0 && (
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
      {view === "done" && (
        <p className="mb-2" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          Threads you marked done are kept here for reference.
        </p>
      )}
      {groupByRepo ? (
        <NotificationsByRepo
          threads={filtered}
          repoName={repoName}
          readOnly={readOnly}
          showRepoMarkRead={view === "inbox"}
          onRead={(id) => readMut.mutate(id)}
          onDone={(id) => doneMut.mutate(id)}
          onSave={(id, saved) => saveMut.mutate({ id, saved })}
          onSubscription={setActiveThread}
          onMarkRepoRead={(fullName) => markRepoMut.mutate(fullName)}
          repoBusy={markRepoMut.isPending}
          busy={busy}
          emptyMessage={emptyMessage}
        />
      ) : (
        <DataTable
          data={filtered ?? []}
          columns={columns}
          filterPlaceholder="Filter notifications…"
          emptyMessage={emptyMessage}
        />
      )}
      {activeThread && (
        <SubscriptionDialog thread={activeThread} onClose={() => setActiveThread(null)} />
      )}
    </>
  );
}

function SaveToggle({
  thread,
  disabled,
  onToggle,
}: {
  thread: GithubNotificationThreadWithSaved;
  disabled: boolean;
  onToggle: (id: string, saved: boolean) => void;
}) {
  const saved = !!thread.saved;
  return (
    <Button
      size="sm"
      variant="ghost"
      aria-pressed={saved}
      aria-label={saved ? `Remove ${thread.subject.title} from saved` : `Save ${thread.subject.title}`}
      disabled={disabled}
      onClick={() => onToggle(thread.id, !saved)}
    >
      {saved ? "Unsave" : "Save"}
    </Button>
  );
}

function RepoAvatar({ repository, size }: { repository: Record<string, unknown>; size: number }) {
  const owner = repository.owner as { login?: string; avatar_url?: string } | undefined;
  if (!owner?.login) return null;
  return <Avatar login={owner.login} src={owner.avatar_url} size={size} />;
}

function NotificationsByRepo({
  threads,
  repoName,
  readOnly,
  showRepoMarkRead,
  onRead,
  onDone,
  onSave,
  onSubscription,
  onMarkRepoRead,
  repoBusy,
  busy,
  emptyMessage,
}: {
  threads: GithubNotificationThreadWithSaved[];
  repoName: (t: GithubNotificationThread) => string;
  readOnly: boolean;
  showRepoMarkRead: boolean;
  onRead: (id: string) => void;
  onDone: (id: string) => void;
  onSave: (id: string, saved: boolean) => void;
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
  const groups = new Map<string, GithubNotificationThreadWithSaved[]>();
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
                <span className="inline-flex items-center gap-1.5" style={{ fontWeight: 600 }}>
                  <RepoAvatar repository={list[0]!.repository} size={18} />
                  {repo}{" "}
                  <span style={{ color: "var(--color-fg-muted)", fontWeight: 400 }}>({list.length})</span>
                </span>
                {showRepoMarkRead && repo.includes("/") && (
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
                          style={{
                            display: "inline-block",
                            color: "var(--color-accent)",
                            textDecoration: "none",
                            lineHeight: "1.625rem",
                            // Wrap long subjects; keeps action buttons above the 24px target-size floor.
                            overflowWrap: "anywhere",
                            maxWidth: "100%",
                          }}
                        >
                          {thread.subject.title}
                        </Link>
                      ) : (
                        <span style={{ overflowWrap: "anywhere" }}>{thread.subject.title}</span>
                      )}
                      <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem", marginLeft: "0.5rem" }}>
                        {thread.subject.type} · {thread.reason} · <RelativeTime iso={thread.updated_at} />
                      </span>
                    </span>
                    {!readOnly && thread.unread && (
                      <Button size="sm" variant="secondary" disabled={busy} onClick={() => onRead(thread.id)}>
                        Mark read
                      </Button>
                    )}
                    <SaveToggle thread={thread} disabled={busy} onToggle={onSave} />
                    {!readOnly && (
                      <Button size="sm" variant="ghost" onClick={() => onSubscription(thread)}>
                        Subscription
                      </Button>
                    )}
                    {!readOnly && (
                      <Button size="sm" variant="ghost" disabled={busy} onClick={() => onDone(thread.id)}>
                        Done
                      </Button>
                    )}
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
  return `/ui/${owner}/${repo}/${path}/${number}`;
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
