import { useState } from "react";
import { useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import {
  fetchCopilotBilling,
  fetchCopilotSeats,
  addCopilotSeats,
  cancelCopilotSeats,
  fetchCopilotSpaces,
  createCopilotSpace,
  updateCopilotSpace,
  deleteCopilotSpace,
  fetchCopilotSpaceCollaborators,
  addCopilotSpaceCollaborator,
  updateCopilotSpaceCollaborator,
  removeCopilotSpaceCollaborator,
  fetchCopilotSpaceResources,
  addCopilotSpaceResource,
  updateCopilotSpaceResource,
  removeCopilotSpaceResource,
  fetchCurrentUser,
  ghFetch,
  ghSend,
  ghPostJSON,
} from "../api.js";
import type { CopilotSpaceOwner } from "../api.js";
import type {
  GithubCopilotSpace,
  GithubCopilotSpaceCollaborator,
  GithubCopilotSpaceResource,
} from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import {
  Box,
  Button,
  DialogActions,
  ErrorBanner,
  FormLabel,
  Modal,
  SectionLabel,
  StatCard,
} from "../components/ui.js";
import { ChevronDownIcon, ChevronRightIcon, CommentIcon } from "../components/octicons.js";
import { Blankslate } from "../components/ui.js";

const enc = encodeURIComponent;

export function CopilotPage() {
  const { org = "" } = useParams<{ org: string }>();

  return (
    <div>
      <OrgHeader org={org} active="copilot" />
      <div className="flex flex-col gap-6">
        <BillingSection org={org} />
        <PolicySection org={org} />
        <UsageSection org={org} />
        <SeatsSection org={org} />
        <ContentExclusionSection org={org} />
        <CodingAgentSection org={org} />
        <SpacesSection owner={org} />
      </div>
    </div>
  );
}

export function PersonalCopilotSpacesPage() {
  const { data: user, isLoading, isError, error } = useQuery({
    queryKey: ["current-user"],
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    staleTime: 60_000,
  });

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 style={{ fontSize: "1.5rem", fontWeight: 600, margin: 0 }}>Your Copilot Spaces</h1>
        <p style={{ color: "var(--color-fg-muted)", marginTop: ".25rem" }}>
          Create personal context collections for Copilot and control who can collaborate on them.
        </p>
      </div>
      {isLoading && <Spinner label="loading your profile" />}
      {isError && <InlineError title="Failed to load your profile" detail={String(error)} />}
      {user && <SpacesSection owner={{ kind: "user", login: user.login }} />}
    </div>
  );
}

function BillingSection({ org }: { org: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["copilot-billing", org],
    queryFn: () => fetchCopilotBilling(org),
  });

  return (
    <section>
      <SectionLabel>Billing</SectionLabel>
      {isLoading && <Spinner label="loading Copilot billing" />}
      {isError && <InlineError title="Failed to load Copilot billing" detail={String(error)} />}
      {data && (
        <>
          <div className="mb-3 grid gap-3 sm:grid-cols-4">
            <StatCard title="Total seats" value={data.seat_breakdown.total} emphasized />
            <StatCard title="Added this cycle" value={data.seat_breakdown.added_this_cycle} />
            <StatCard title="Pending invitation" value={data.seat_breakdown.pending_invitation} />
            <StatCard title="Pending cancellation" value={data.seat_breakdown.pending_cancellation} />
          </div>
          <Box>
            <div className="flex flex-wrap gap-x-6 gap-y-1" style={{ padding: "0.75rem 1rem", fontSize: "0.85rem" }}>
              <span>
                Plan: <strong>{data.plan_type}</strong>
              </span>
              <span>
                Seat management: <strong>{data.seat_management_setting}</strong>
              </span>
              <span>
                Public code suggestions: <strong>{data.public_code_suggestions}</strong>
              </span>
              <span>
                IDE chat: <strong>{data.ide_chat}</strong>
              </span>
              <span>
                Platform chat: <strong>{data.platform_chat}</strong>
              </span>
              <span>
                CLI: <strong>{data.cli}</strong>
              </span>
            </div>
          </Box>
        </>
      )}
    </section>
  );
}

/** The Copilot subscription policy an organization owner configures. */
export interface CopilotPolicy {
  plan_type: string;
  seat_management_setting: string;
  public_code_suggestions: string;
  ide_chat: string;
  platform_chat: string;
  cli: string;
}

/** One day of aggregated Copilot usage, as the metrics endpoint reports it. */
interface CopilotMetricsDay {
  date: string;
  total_active_users: number;
  total_engaged_users: number;
  copilot_ide_code_completions: {
    total_engaged_users: number;
    editors: {
      name: string;
      total_engaged_users: number;
      models: { name: string; languages: { name: string; total_code_suggestions: number; total_code_acceptances: number }[] }[];
    }[];
  };
  copilot_ide_chat: { total_engaged_users: number; editors: { models: { total_chats: number }[] }[] };
}

// The policy and usage wrappers live in this lazy page: api.ts is reachable
// from the entry chunk, which sits against its 160 KB budget.
const fetchCopilotPolicy = (org: string, signal?: AbortSignal) =>
  ghFetch<CopilotPolicy>(`/ui-data/orgs/${enc(org)}/copilot/policy`, signal);
const fetchCopilotMetrics = (org: string, signal?: AbortSignal) =>
  ghFetch<CopilotMetricsDay[]>(`/api/v3/orgs/${enc(org)}/copilot/metrics`, signal);

const COPILOT_POLICY_FIELDS: { key: keyof CopilotPolicy; label: string; options: string[] }[] = [
  { key: "plan_type", label: "Plan", options: ["business", "enterprise", "individual", "unknown"] },
  { key: "seat_management_setting", label: "Seat management", options: ["assign_selected", "assign_all", "disabled", "unconfigured"] },
  { key: "public_code_suggestions", label: "Public code suggestions", options: ["allow", "block", "unconfigured"] },
  { key: "ide_chat", label: "IDE chat", options: ["enabled", "disabled", "unconfigured"] },
  { key: "platform_chat", label: "Platform chat", options: ["enabled", "disabled", "unconfigured"] },
  { key: "cli", label: "CLI", options: ["enabled", "disabled", "unconfigured"] },
];

function PolicySection({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["copilot-policy", org],
    queryFn: ({ signal }) => fetchCopilotPolicy(org, signal),
  });
  const save = useMutation({
    mutationFn: (patch: Partial<CopilotPolicy>) => ghSend("PUT", `/ui-data/orgs/${enc(org)}/copilot/policy`, patch),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["copilot-policy", org] });
      qc.invalidateQueries({ queryKey: ["copilot-billing", org] });
      qc.invalidateQueries({ queryKey: ["copilot-seats", org] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <section>
      <SectionLabel>Policy</SectionLabel>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {isLoading && <Spinner label="loading Copilot policy" />}
      {isError && <InlineError title="Failed to load Copilot policy" detail={String(loadErr)} />}
      {data && (
        <Box>
          <div className="grid gap-3 sm:grid-cols-3" style={{ padding: "0.75rem 1rem" }}>
            {COPILOT_POLICY_FIELDS.map((field) => (
              <div key={field.key}>
                <FormLabel id={`copilot-policy-${field.key}`}>{field.label}</FormLabel>
                <select
                  id={`copilot-policy-${field.key}`}
                  className="w-full"
                  value={data[field.key]}
                  disabled={save.isPending}
                  onChange={(e) => save.mutate({ [field.key]: e.target.value } as Partial<CopilotPolicy>)}
                >
                  {field.options.map((option) => (
                    <option key={option} value={option}>{option}</option>
                  ))}
                </select>
              </div>
            ))}
          </div>
        </Box>
      )}
    </section>
  );
}

function UsageSection({ org }: { org: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["copilot-metrics", org],
    queryFn: ({ signal }) => fetchCopilotMetrics(org, signal),
  });

  return (
    <section>
      <SectionLabel>Usage</SectionLabel>
      {isLoading && <Spinner label="loading Copilot usage" />}
      {isError && <InlineError title="Failed to load Copilot usage" detail={String(error)} />}
      {data && data.length === 0 && (
        <Blankslate title="No Copilot activity recorded">
          Metrics appear once members use their seats; nothing is reported that did not happen.
        </Blankslate>
      )}
      {data && data.length > 0 && (
        <Box>
          <div style={{ overflowX: "auto" }}>
          <table className="w-full" style={{ fontSize: "0.9rem", minWidth: "30rem" }}>
            <caption className="sr-only">Daily Copilot usage</caption>
            <thead>
              <tr>
                <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Day</th>
                <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Active users</th>
                <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Suggestions</th>
                <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Acceptances</th>
                <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Chats</th>
              </tr>
            </thead>
            <tbody>
              {data.map((day) => {
                let suggestions = 0;
                let acceptances = 0;
                for (const editor of day.copilot_ide_code_completions.editors) {
                  for (const model of editor.models) {
                    for (const language of model.languages) {
                      suggestions += language.total_code_suggestions;
                      acceptances += language.total_code_acceptances;
                    }
                  }
                }
                const chats = day.copilot_ide_chat.editors.reduce(
                  (total, editor) => total + editor.models.reduce((sum, model) => sum + model.total_chats, 0),
                  0,
                );
                return (
                  <tr key={day.date}>
                    <td style={{ padding: "0.5rem 1rem" }}>{day.date}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{day.total_active_users}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{suggestions}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{acceptances}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{chats}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          </div>
        </Box>
      )}
    </section>
  );
}

function SeatsSection({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [usernames, setUsernames] = useState("");
  const [teamSlug, setTeamSlug] = useState("");

  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["copilot-seats", org],
    queryFn: ({ signal }) => fetchCopilotSeats(org, signal),
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["copilot-seats", org] });
    qc.invalidateQueries({ queryKey: ["copilot-billing", org] });
  };
  const addMut = useMutation({
    mutationFn: () =>
      addCopilotSeats(
        org,
        usernames
          .split(",")
          .map((u) => u.trim())
          .filter(Boolean),
      ),
    onSuccess: () => {
      invalidate();
      setUsernames("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const removeMut = useMutation({
    mutationFn: (login: string) => cancelCopilotSeats(org, [login]),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  // github.com also lets you grant/revoke Copilot seats for a whole team by slug.
  const addTeamMut = useMutation({
    mutationFn: () =>
      ghPostJSON<{ seats_created: number }>(
        `/api/v3/orgs/${enc(org)}/copilot/billing/selected_teams`,
        { selected_teams: [teamSlug.trim()] },
      ),
    onSuccess: () => {
      invalidate();
      setTeamSlug("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const removeTeamMut = useMutation({
    mutationFn: () =>
      ghSend("DELETE", `/api/v3/orgs/${enc(org)}/copilot/billing/selected_teams`, {
        selected_teams: [teamSlug.trim()],
      }),
    onSuccess: () => {
      invalidate();
      setTeamSlug("");
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <section>
      <SectionLabel>Seats</SectionLabel>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box>
        <div className="flex gap-2" style={{ padding: "0.75rem 1rem", borderBottom: "1px solid var(--color-border)" }}>
          <input
            aria-label="Usernames to assign Copilot seats"
            placeholder="usernames, comma-separated"
            value={usernames}
            onChange={(e) => setUsernames(e.target.value)}
            className="w-full"
          />
          <Button
            variant="primary"
            size="sm"
            disabled={addMut.isPending || !usernames.trim()}
            onClick={() => {
              setError(null);
              addMut.mutate();
            }}
          >
            Assign seats
          </Button>
        </div>
        <div className="flex gap-2" style={{ padding: "0.75rem 1rem", borderBottom: "1px solid var(--color-border)" }}>
          <input
            aria-label="Team slug to assign Copilot seats"
            placeholder="team-slug"
            value={teamSlug}
            onChange={(e) => setTeamSlug(e.target.value)}
            className="w-full"
          />
          <Button
            variant="primary"
            size="sm"
            disabled={addTeamMut.isPending || !teamSlug.trim()}
            onClick={() => {
              setError(null);
              addTeamMut.mutate();
            }}
          >
            Add team
          </Button>
          <Button
            variant="danger"
            size="sm"
            disabled={removeTeamMut.isPending || !teamSlug.trim()}
            onClick={() => {
              setError(null);
              removeTeamMut.mutate();
            }}
          >
            Remove team
          </Button>
        </div>
        {isLoading && <Spinner label="loading Copilot seats" />}
        {isError && <InlineError title="Failed to load Copilot seats" detail={String(loadErr)} />}
        {data &&
          (data.seats.length === 0 ? (
            <div style={{ padding: "0.9rem 1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
              No Copilot seats assigned.
            </div>
          ) : (
            data.seats.map((seat, i) => {
              const login = seat.assignee?.login;
              return (
              <div
                key={login ?? i}
                className="flex flex-wrap items-center justify-between gap-3"
                style={{
                  padding: "0.6rem 1rem",
                  borderBottom: i < data.seats.length - 1 ? "1px solid var(--color-border)" : "none",
                }}
              >
                <span style={{ fontWeight: 500 }}>
                  {login ? `@${login}` : "(unresolved assignee)"}
                </span>
                <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
                  {seat.plan_type} · since {new Date(seat.created_at).toLocaleDateString()}
                  {seat.assigning_team && ` · via team @${seat.assigning_team.slug}`}
                  {seat.pending_cancellation_date && ` · cancels ${seat.pending_cancellation_date}`}
                </span>
                {login && (
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={removeMut.isPending}
                    onClick={async () => {
                      if (await confirmAction(`Cancel ${login}'s Copilot seat?`)) {
                        removeMut.mutate(login);
                      }
                    }}
                  >
                    cancel seat
                  </Button>
                )}
              </div>
              );
            })
          ))}
      </Box>
    </section>
  );
}

// Copilot content exclusion: an organization maps each scope (a repository
// "owner/name" or "*" for all) to a list of rules, each a path string or an
// object with exactly one of ifAnyMatch / ifNoneMatch. The shape is faithfully
// edited as JSON and PUT back to the org content_exclusion endpoint.
function ContentExclusionSection({ org }: { org: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["copilot-content-exclusion", org],
    queryFn: ({ signal }) =>
      ghFetch<Record<string, unknown[]>>(
        `/api/v3/orgs/${enc(org)}/copilot/content_exclusion`,
        signal,
      ),
  });

  return (
    <section>
      <SectionLabel>Content exclusion</SectionLabel>
      {isLoading && <Spinner label="loading content exclusion rules" />}
      {isError && (
        <InlineError title="Failed to load content exclusion rules" detail={String(error)} />
      )}
      {/* Keyed on org so the editor seeds its text from the loaded rules once. */}
      {data && <ContentExclusionEditor key={org} org={org} initial={data} />}
    </section>
  );
}

function ContentExclusionEditor({
  org,
  initial,
}: {
  org: string;
  initial: Record<string, unknown[]>;
}) {
  const qc = useQueryClient();
  const [text, setText] = useState(() => JSON.stringify(initial, null, 2));
  const [error, setError] = useState<string | null>(null);

  const saveMut = useMutation({
    mutationFn: () => {
      let parsed: unknown;
      try {
        parsed = text.trim() === "" ? {} : JSON.parse(text);
      } catch {
        throw new Error("Rules must be valid JSON.");
      }
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
        throw new Error("Rules must be a JSON object mapping each scope to a list of rules.");
      }
      return ghSend("PUT", `/api/v3/orgs/${enc(org)}/copilot/content_exclusion`, parsed);
    },
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["copilot-content-exclusion", org] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Box>
      <div className="flex flex-col gap-2" style={{ padding: "0.75rem 1rem" }}>
        <p style={{ margin: 0, fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
          Map each scope (a repository <code>owner/name</code>, or <code>*</code> for all) to a list
          of rules. Each rule is a path string or an object with exactly one of{" "}
          <code>ifAnyMatch</code> / <code>ifNoneMatch</code>.
        </p>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <textarea
          aria-label="Copilot content exclusion rules"
          value={text}
          onChange={(e) => setText(e.target.value)}
          rows={8}
          className="w-full"
          style={{ fontFamily: "var(--font-mono, monospace)", fontSize: "0.82rem" }}
        />
        <div>
          <Button
            variant="primary"
            size="sm"
            disabled={saveMut.isPending}
            onClick={() => {
              setError(null);
              saveMut.mutate();
            }}
          >
            {saveMut.isPending ? "Saving…" : "Save exclusion rules"}
          </Button>
        </div>
      </div>
    </Box>
  );
}

const CODING_AGENT_POLICIES = ["all", "selected", "none"] as const;

// Copilot coding agent: the organization policy for which repositories may use
// the Copilot cloud/coding agent — all, a selected set, or none.
function CodingAgentSection({ org }: { org: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["copilot-coding-agent-permissions", org],
    queryFn: ({ signal }) =>
      ghFetch<{ enabled_repositories: string }>(
        `/api/v3/orgs/${enc(org)}/copilot/coding-agent/permissions`,
        signal,
      ),
  });

  return (
    <section>
      <SectionLabel>Coding agent</SectionLabel>
      {isLoading && <Spinner label="loading Copilot coding agent policy" />}
      {isError && (
        <InlineError title="Failed to load Copilot coding agent policy" detail={String(error)} />
      )}
      {/* Keyed on org so the select seeds its value from the loaded policy once. */}
      {data && <CodingAgentEditor key={org} org={org} initial={data.enabled_repositories} />}
    </section>
  );
}

function CodingAgentEditor({ org, initial }: { org: string; initial: string }) {
  const qc = useQueryClient();
  const [policy, setPolicy] = useState(initial);
  const [error, setError] = useState<string | null>(null);

  const saveMut = useMutation({
    mutationFn: () =>
      ghSend("PUT", `/api/v3/orgs/${enc(org)}/copilot/coding-agent/permissions`, {
        enabled_repositories: policy,
      }),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["copilot-coding-agent-permissions", org] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Box>
      <div className="flex flex-wrap items-center gap-3" style={{ padding: "0.75rem 1rem" }}>
        <FormLabel id="coding-agent-policy">
          Repositories allowed to use Copilot coding agent
        </FormLabel>
        <select
          id="coding-agent-policy"
          aria-label="Copilot coding agent repository policy"
          value={policy}
          onChange={(e) => setPolicy(e.target.value)}
        >
          {CODING_AGENT_POLICIES.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
        <Button
          variant="primary"
          size="sm"
          disabled={saveMut.isPending}
          onClick={() => {
            setError(null);
            saveMut.mutate();
          }}
        >
          {saveMut.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
    </Box>
  );
}

function ownerKey(owner: CopilotSpaceOwner): string {
  return typeof owner === "string" ? `organization:${owner}` : `${owner.kind}:${owner.login}`;
}

function SpacesSection({ owner }: { owner: CopilotSpaceOwner }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<GithubCopilotSpace | null>(null);

  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["copilot-spaces", ownerKey(owner)],
    queryFn: ({ signal }) => fetchCopilotSpaces(owner, signal),
  });

  const deleteMut = useMutation({
    mutationFn: (spaceNumber: number) => deleteCopilotSpace(owner, spaceNumber),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["copilot-spaces", ownerKey(owner)] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <section>
      <div className="flex items-center justify-between gap-3">
        <SectionLabel>Copilot Spaces</SectionLabel>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New space
        </Button>
      </div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {isLoading && <Spinner label="loading Copilot Spaces" />}
      {isError && <InlineError title="Failed to load Copilot Spaces" detail={String(loadErr)} />}
      {data &&
        (data.length === 0 ? (
          <Blankslate icon={<CommentIcon size={26} />} title="No Copilot Spaces">
            Spaces bundle repositories, files, and instructions into a shared Copilot context.
          </Blankslate>
        ) : (
          <div className="flex flex-col gap-2">
            {data.map((space) => (
              <SpaceCard
                key={space.id}
                owner={owner}
                space={space}
                onEdit={() => setEditing(space)}
                onDelete={async () => {
                  if (await confirmAction(`Delete Copilot Space ${space.name}?`)) deleteMut.mutate(space.number);
                }}
                deleting={deleteMut.isPending}
              />
            ))}
          </div>
        ))}
      {(creating || editing) && (
        <SpaceDialog
          owner={owner}
          space={editing ?? undefined}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      )}
    </section>
  );
}

function SpaceCard({
  owner,
  space,
  onEdit,
  onDelete,
  deleting,
}: {
  owner: CopilotSpaceOwner;
  space: GithubCopilotSpace;
  onEdit: () => void;
  onDelete: () => void;
  deleting: boolean;
}) {
  const [open, setOpen] = useState(false);
  return (
    <Box>
      <div className="flex flex-wrap items-center gap-3" style={{ padding: "0.6rem 1rem" }}>
        <button
          type="button"
          className="flex min-w-0 flex-1 items-center gap-2 text-left"
          onClick={() => setOpen((v) => !v)}
          style={{ background: "transparent", border: "none", color: "var(--color-fg)", padding: 0 }}
        >
          {open ? <ChevronDownIcon size={14} /> : <ChevronRightIcon size={14} />}
          <span style={{ fontWeight: 600, fontSize: "0.9rem" }}>
            {space.name}
            <span style={{ color: "var(--color-fg-muted)", fontWeight: 400 }}> #{space.number}</span>
          </span>
          <span className="min-w-0 flex-1" style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
            {space.description ?? "No description"}
          </span>
          <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
            base role: {space.base_role}
            {space.creator && ` · created by @${space.creator.login}`} ·{" "}
            {new Date(space.updated_at).toLocaleDateString()}
          </span>
        </button>
        <Button size="sm" variant="ghost" onClick={onEdit}>
          edit
        </Button>
        <Button size="sm" variant="danger" disabled={deleting} onClick={onDelete}>
          delete
        </Button>
      </div>
      {open && (
        <div
          className="flex flex-col gap-4"
          style={{ borderTop: "1px solid var(--color-border)", padding: "0.75rem 1rem" }}
        >
          {space.general_instructions && (
            <div style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
              {space.general_instructions}
            </div>
          )}
          <SpaceCollaboratorsPanel owner={owner} spaceNumber={space.number} />
          <SpaceResourcesPanel owner={owner} spaceNumber={space.number} />
        </div>
      )}
    </Box>
  );
}

const COPILOT_SPACE_ROLES = ["reader", "writer", "admin"];

function SpaceDialog({
  owner,
  space,
  onClose,
}: {
  owner: CopilotSpaceOwner;
  space?: GithubCopilotSpace | undefined;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(space?.name ?? "");
  const [description, setDescription] = useState(space?.description ?? "");
  const [instructions, setInstructions] = useState(space?.general_instructions ?? "");
  const [baseRole, setBaseRole] = useState(space?.base_role ?? "no_access");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => {
      const payload = {
        name: name.trim(),
        description,
        general_instructions: instructions,
        base_role: baseRole,
      };
      return space ? updateCopilotSpace(owner, space.number, payload) : createCopilotSpace(owner, payload);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["copilot-spaces", ownerKey(owner)] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={space ? `Edit ${space.name}` : "New Copilot Space"} onClose={onClose}>
      <FormLabel id="space-name">Name</FormLabel>
      <input
        id="space-name"
        autoFocus
        value={name}
        onChange={(e) => setName(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="space-desc">Description (optional)</FormLabel>
      <input
        id="space-desc"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="space-instructions">General instructions (optional)</FormLabel>
      <textarea
        id="space-instructions"
        value={instructions}
        onChange={(e) => setInstructions(e.target.value)}
        rows={3}
        className="mb-3 w-full"
      />
      {typeof owner === "string" || owner.kind === "organization" ? (
        <>
          <FormLabel id="space-base-role">Base role for organization members</FormLabel>
          <select
            id="space-base-role"
            value={baseRole}
            onChange={(e) => setBaseRole(e.target.value)}
            className="mb-4 w-full"
          >
            <option value="no_access">no_access</option>
            {COPILOT_SPACE_ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </>
      ) : null}
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mutation.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!name.trim() || mutation.isPending}
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
        >
          {mutation.isPending ? "Saving…" : space ? "Save" : "Create space"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

/** The path identifier the collaborator endpoints key on: login for users, slug for teams. */
function collaboratorIdentifier(c: GithubCopilotSpaceCollaborator): string {
  return (c.actor_type === "Team" ? c.slug : c.login) ?? String(c.id);
}

function SpaceCollaboratorsPanel({
  owner,
  spaceNumber,
}: {
  owner: CopilotSpaceOwner;
  spaceNumber: number;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [actorType, setActorType] = useState<"User" | "Team">("User");
  const [identifier, setIdentifier] = useState("");
  const [role, setRole] = useState("reader");

  const key = ["copilot-space-collaborators", ownerKey(owner), spaceNumber];
  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => fetchCopilotSpaceCollaborators(owner, spaceNumber, signal),
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: key });
  };
  const addMut = useMutation({
    mutationFn: () =>
      addCopilotSpaceCollaborator(owner, spaceNumber, {
        actor_type: actorType,
        actor_identifier: identifier.trim(),
        role,
      }),
    onSuccess: () => {
      invalidate();
      setIdentifier("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const roleMut = useMutation({
    mutationFn: ({ c, newRole }: { c: GithubCopilotSpaceCollaborator; newRole: string }) =>
      updateCopilotSpaceCollaborator(owner, spaceNumber, c.actor_type, collaboratorIdentifier(c), newRole),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const removeMut = useMutation({
    mutationFn: (c: GithubCopilotSpaceCollaborator) =>
      removeCopilotSpaceCollaborator(owner, spaceNumber, c.actor_type, collaboratorIdentifier(c)),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });

  return (
    <div>
      <div style={{ fontSize: "0.8rem", fontWeight: 600, marginBottom: "0.4rem" }}>Collaborators</div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {isLoading && <Spinner label="loading space collaborators" />}
      {isError && <InlineError title="Failed to load collaborators" detail={String(loadErr)} />}
      {data &&
        (data.length === 0 ? (
          <div style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
            No collaborators granted beyond the base role.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {data.map((c) => {
              const label = c.actor_type === "Team" ? `@${c.slug} (team)` : `@${c.login}`;
              return (
                <li
                  key={`${c.actor_type}-${c.id}`}
                  className="flex flex-wrap items-center justify-between gap-2 py-1"
                  style={{ fontSize: "0.85rem" }}
                >
                  <span style={{ fontWeight: 500 }}>{label}</span>
                  <span className="flex items-center gap-2">
                    <select
                      aria-label={`Role for ${label}`}
                      value={c.role}
                      disabled={roleMut.isPending}
                      onChange={(e) => roleMut.mutate({ c, newRole: e.target.value })}
                    >
                      {COPILOT_SPACE_ROLES.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </select>
                    <Button
                      size="sm"
                      variant="danger"
                      disabled={removeMut.isPending}
                      onClick={() => removeMut.mutate(c)}
                    >
                      remove
                    </Button>
                  </span>
                </li>
              );
            })}
          </ul>
        ))}
      <div className="mt-2 flex flex-wrap gap-2">
        <select
          aria-label="Collaborator actor type"
          value={actorType}
          onChange={(e) => setActorType(e.target.value as "User" | "Team")}
        >
          <option value="User">User</option>
          {(typeof owner === "string" || owner.kind === "organization") && <option value="Team">Team</option>}
        </select>
        <input
          aria-label="Collaborator username or team slug"
          placeholder={actorType === "Team" ? "team-slug" : "username"}
          value={identifier}
          onChange={(e) => setIdentifier(e.target.value)}
          className="min-w-0 flex-1"
        />
        <select aria-label="Collaborator role" value={role} onChange={(e) => setRole(e.target.value)}>
          {COPILOT_SPACE_ROLES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
        <Button
          size="sm"
          disabled={addMut.isPending || !identifier.trim()}
          onClick={() => {
            setError(null);
            addMut.mutate();
          }}
        >
          Add
        </Button>
      </div>
    </div>
  );
}

const SPACE_RESOURCE_TYPES = [
  "repository",
  "github_file",
  "github_issue",
  "github_pull_request",
  "free_text",
  "media_content",
  "uploaded_text_file",
] as const;

type SpaceResourceType = (typeof SPACE_RESOURCE_TYPES)[number];

function resourceSummary(res: GithubCopilotSpaceResource): string {
  const m = res.metadata;
  switch (res.resource_type) {
    case "repository":
      return `repository #${String(m.repository_id)}`;
    case "github_file":
      return `file ${String(m.file_path)} in repository #${String(m.repository_id)}`;
    case "github_issue":
      return `issue #${String(m.number)} in repository #${String(m.repository_id)}`;
    case "github_pull_request":
      return `pull request #${String(m.number)} in repository #${String(m.repository_id)}`;
    case "free_text":
      return String(m.text);
    default:
      return JSON.stringify(m);
  }
}

function SpaceResourcesPanel({ owner, spaceNumber }: { owner: CopilotSpaceOwner; spaceNumber: number }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<GithubCopilotSpaceResource | null>(null);

  const key = ["copilot-space-resources", ownerKey(owner), spaceNumber];
  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => fetchCopilotSpaceResources(owner, spaceNumber, signal),
  });

  const removeMut = useMutation({
    mutationFn: (resourceId: number) => removeCopilotSpaceResource(owner, spaceNumber, resourceId),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: key });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <div>
      <div style={{ fontSize: "0.8rem", fontWeight: 600, marginBottom: "0.4rem" }}>Resources</div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {isLoading && <Spinner label="loading space resources" />}
      {isError && <InlineError title="Failed to load resources" detail={String(loadErr)} />}
      {data &&
        (data.length === 0 ? (
          <div style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
            No resources attached.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {data.map((res) => (
              <li
                key={res.id}
                className="flex flex-wrap items-center justify-between gap-2 py-1"
                style={{ fontSize: "0.85rem" }}
              >
                <span className="min-w-0 flex-1">
                  <span style={{ fontWeight: 500 }}>{res.resource_type}</span>{" "}
                  <span style={{ color: "var(--color-fg-muted)" }}>{resourceSummary(res)}</span>
                </span>
                <Button size="sm" variant="ghost" onClick={() => setEditing(res)}>
                  edit
                </Button>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={removeMut.isPending}
                  onClick={() => removeMut.mutate(res.id)}
                >
                  remove
                </Button>
              </li>
            ))}
          </ul>
        ))}
      {/* Keyed on the edited resource so switching rows remounts with fresh fields. */}
      <SpaceResourceForm
        key={editing?.id ?? "new"}
        owner={owner}
        spaceNumber={spaceNumber}
        resource={editing ?? undefined}
        onDone={() => setEditing(null)}
      />
    </div>
  );
}

function SpaceResourceForm({
  owner,
  spaceNumber,
  resource,
  onDone,
}: {
  owner: CopilotSpaceOwner;
  spaceNumber: number;
  /** When set the form edits this resource's metadata (the type is fixed). */
  resource?: GithubCopilotSpaceResource | undefined;
  onDone: () => void;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [resourceType, setResourceType] = useState<SpaceResourceType>("repository");
  const [repositoryId, setRepositoryId] = useState(
    resource?.metadata.repository_id != null ? String(resource.metadata.repository_id) : "",
  );
  const [filePath, setFilePath] = useState(
    typeof resource?.metadata.file_path === "string" ? resource.metadata.file_path : "",
  );
  const [itemNumber, setItemNumber] = useState(
    resource?.metadata.number != null ? String(resource.metadata.number) : "",
  );
  const [text, setText] = useState(
    typeof resource?.metadata.text === "string" ? resource.metadata.text : "",
  );
  const [rawMetadata, setRawMetadata] = useState(
    JSON.stringify(resource?.metadata ?? {}, null, 2),
  );

  const effectiveType = resource ? (resource.resource_type as SpaceResourceType) : resourceType;
  const needsRepo =
    effectiveType === "repository" ||
    effectiveType === "github_file" ||
    effectiveType === "github_issue" ||
    effectiveType === "github_pull_request";
  const needsPath = effectiveType === "github_file";
  const needsNumber = effectiveType === "github_issue" || effectiveType === "github_pull_request";
  const needsText = effectiveType === "free_text";
  const needsRawMetadata =
    effectiveType === "media_content" || effectiveType === "uploaded_text_file";
  let parsedRawMetadata: Record<string, unknown> | undefined;
  if (needsRawMetadata) {
    try {
      const parsed: unknown = JSON.parse(rawMetadata);
      if (typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)) {
        parsedRawMetadata = parsed as Record<string, unknown>;
      }
    } catch {
      // The invalid state is reflected in `valid` and keeps submission disabled.
    }
  }

  const metadata = (): Record<string, unknown> => {
    if (needsRawMetadata) return parsedRawMetadata ?? {};
    const m: Record<string, unknown> = {};
    if (needsRepo) m.repository_id = parseInt(repositoryId, 10);
    if (needsPath) m.file_path = filePath.trim();
    if (needsNumber) m.number = parseInt(itemNumber, 10);
    if (needsText) m.text = text;
    return m;
  };
  const valid =
    (!needsRepo || Number.isFinite(parseInt(repositoryId, 10))) &&
    (!needsPath || filePath.trim() !== "") &&
    (!needsNumber || Number.isFinite(parseInt(itemNumber, 10))) &&
    (!needsText || text.trim() !== "") &&
    (!needsRawMetadata || parsedRawMetadata !== undefined);

  const saveMut = useMutation({
    mutationFn: () =>
      resource
        ? updateCopilotSpaceResource(owner, spaceNumber, resource.id, metadata())
        : addCopilotSpaceResource(owner, spaceNumber, { resource_type: effectiveType, metadata: metadata() }),
    onSuccess: () => {
      setError(null);
      setRepositoryId("");
      setFilePath("");
      setItemNumber("");
      setText("");
      setRawMetadata("{}");
      qc.invalidateQueries({ queryKey: ["copilot-space-resources", ownerKey(owner), spaceNumber] });
      onDone();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <div className="mt-2">
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <div className="flex flex-wrap gap-2">
        <select
          aria-label="Resource type"
          value={effectiveType}
          disabled={!!resource}
          onChange={(e) => setResourceType(e.target.value as SpaceResourceType)}
        >
          {SPACE_RESOURCE_TYPES.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </select>
        {needsRepo && (
          <input
            aria-label="Resource repository ID"
            type="number"
            min={1}
            placeholder="repository ID"
            value={repositoryId}
            onChange={(e) => setRepositoryId(e.target.value)}
            style={{ width: "9rem" }}
          />
        )}
        {needsPath && (
          <input
            aria-label="Resource file path"
            placeholder="path/to/file"
            value={filePath}
            onChange={(e) => setFilePath(e.target.value)}
            className="min-w-0 flex-1"
          />
        )}
        {needsNumber && (
          <input
            aria-label={effectiveType === "github_issue" ? "Resource issue number" : "Resource pull request number"}
            type="number"
            min={1}
            placeholder="number"
            value={itemNumber}
            onChange={(e) => setItemNumber(e.target.value)}
            style={{ width: "7rem" }}
          />
        )}
        {needsText && (
          <input
            aria-label="Resource free text"
            placeholder="free text"
            value={text}
            onChange={(e) => setText(e.target.value)}
            className="min-w-0 flex-1"
          />
        )}
        {needsRawMetadata && (
          <textarea
            aria-label="Resource metadata JSON"
            value={rawMetadata}
            onChange={(e) => setRawMetadata(e.target.value)}
            rows={4}
            className="min-w-0 flex-1"
          />
        )}
        <Button
          size="sm"
          disabled={saveMut.isPending || !valid}
          onClick={() => {
            setError(null);
            saveMut.mutate();
          }}
        >
          {resource ? "Save resource" : "Attach"}
        </Button>
        {resource && (
          <Button size="sm" variant="ghost" disabled={saveMut.isPending} onClick={onDone}>
            Cancel
          </Button>
        )}
      </div>
    </div>
  );
}
