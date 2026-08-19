import { Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import type { BleephubRepo } from "../types.js";
import { languageColor } from "../utils/langColors.js";
import { limitedGhFetch } from "../utils/uiFetch.js";
import { StarIcon, RepoForkedIcon } from "./octicons.js";
import { RelativeTime } from "./RelativeTime.js";

/*
 * The GitHub repo-card meta strip: language dot, star count (linking to the
 * stargazers list), fork count and last-updated time — shared by every repo
 * row/card (repo lists, profile Repositories/Stars, pinned cards, org
 * overview). Plus the lazy "Forked from owner/name" line for forks.
 */

// Small standalone links need an inline-block box with ≥1.625rem line-height
// to clear the WCAG 2.2 target-size ratchet.
const smallLink = {
  color: "var(--color-fg-muted)",
  textDecoration: "none",
  display: "inline-block",
  lineHeight: "1.625rem",
} as const;

export function LanguageDot({ language }: { language: string }) {
  return (
    <span className="inline-flex items-center gap-1">
      <span
        aria-hidden="true"
        style={{
          width: "10px",
          height: "10px",
          borderRadius: "50%",
          background: languageColor(language),
          display: "inline-block",
          flexShrink: 0,
        }}
      />
      {language}
    </span>
  );
}

/** Language dot + stars (→ stargazers) + forks (+ updated time unless showUpdated={false}). */
export function RepoStatsLine({
  repo,
  showUpdated = true,
}: {
  repo: BleephubRepo;
  showUpdated?: boolean;
}) {
  const [owner, name] = repo.full_name.split("/");
  const stars = repo.stargazers_count ?? 0;
  const forks = repo.forks_count ?? 0;
  return (
    <div
      className="flex flex-wrap items-center gap-x-4 gap-y-1"
      style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}
    >
      {repo.language && <LanguageDot language={repo.language} />}
      <Link
        to={`/ui/repos/${owner}/${name}/stargazers`}
        aria-label={`${stars} star${stars === 1 ? "" : "s"}`}
        style={smallLink}
      >
        <span className="inline-flex items-center gap-1">
          <StarIcon size={14} /> {stars}
        </span>
      </Link>
      <span className="inline-flex items-center gap-1" aria-label={`${forks} fork${forks === 1 ? "" : "s"}`}>
        <RepoForkedIcon size={14} /> {forks}
      </span>
      {showUpdated && (
        <span>
          Updated <RelativeTime iso={repo.updated_at} />
        </span>
      )}
    </div>
  );
}

/**
 * "Forked from owner/name" line under a fork's card. List payloads carry only
 * the `fork` flag (parent rides only on the full-repo response, as on real
 * GitHub), so the parent is hydrated lazily — concurrency-capped and cached
 * forever per repo — only for rows where fork === true.
 */
export function ForkedFromLine({ repo }: { repo: BleephubRepo }) {
  const isFork = repo.fork === true;
  const [owner, name] = repo.full_name.split("/");
  const { data } = useQuery({
    queryKey: ["repo-fork-parent", repo.full_name],
    queryFn: () =>
      limitedGhFetch<{ parent?: { full_name: string } }>(
        `/api/v3/repos/${encodeURIComponent(owner ?? "")}/${encodeURIComponent(name ?? "")}`,
      ),
    enabled: isFork,
    staleTime: Infinity,
    retry: false,
  });
  const parent = data?.parent;
  if (!isFork || !parent?.full_name) return null;
  const [po, pn] = parent.full_name.split("/");
  return (
    <div style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
      Forked from{" "}
      <Link to={`/ui/repos/${po}/${pn}`} style={smallLink}>
        {parent.full_name}
      </Link>
    </div>
  );
}

// ─── Octicons the shared set lacks (profile/org meta rows) ──────────────────

export function LocationIcon({ size = 15 }: { size?: number }) {
  return (
    <svg aria-hidden="true" width={size} height={size} viewBox="0 0 16 16" fill="currentColor">
      <path d="m12.596 11.596-3.535 3.536a1.5 1.5 0 0 1-2.122 0l-3.535-3.536a6.5 6.5 0 1 1 9.192-9.193 6.5 6.5 0 0 1 0 9.193Zm-1.06-8.132v-.001a5 5 0 1 0-7.072 7.072L8 14.07l3.536-3.534a5 5 0 0 0 0-7.072ZM8 9a2 2 0 1 1-.001-3.999A2 2 0 0 1 8 9Z" />
    </svg>
  );
}

export function MailIcon({ size = 15 }: { size?: number }) {
  return (
    <svg aria-hidden="true" width={size} height={size} viewBox="0 0 16 16" fill="currentColor">
      <path d="M1.75 2h12.5c.966 0 1.75.784 1.75 1.75v8.5A1.75 1.75 0 0 1 14.25 14H1.75A1.75 1.75 0 0 1 0 12.25v-8.5C0 2.784.784 2 1.75 2ZM1.5 12.251c0 .138.112.25.25.25h12.5a.25.25 0 0 0 .25-.25V5.809L8.38 9.397a.75.75 0 0 1-.76 0L1.5 5.809v6.442Zm13-8.181v-.32a.25.25 0 0 0-.25-.25H1.75a.25.25 0 0 0-.25.25v.32L8 7.88Z" />
    </svg>
  );
}
