import { useState } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { ghFetch } from "../api.js";
import { Blankslate, Box, Button, ButtonLink, PageTitle } from "../components/ui.js";
import { LockIcon } from "../components/octicons.js";
import { RelativeTime } from "../components/RelativeTime.js";

/**
 * The global security advisory database — the public catalogue of every
 * advisory published on this instance, and the detail view for one of them.
 *
 * This is deliberately NOT a repository page. An advisory is drafted inside a
 * repository and is private while it is; publishing it moves it here, where it
 * is a public fact about a package rather than about the repository that
 * happened to find it. Nothing on these two views is repository-scoped, and
 * neither view can reach an unpublished advisory.
 *
 * The fetch wrappers live in this lazy page rather than in api.ts on purpose:
 * the entry chunk sits against its size budget, and a new wrapper there is
 * paid for by every route.
 */

const enc = encodeURIComponent;

/** One identifier (GHSA or CVE) an advisory is known by. */
interface AdvisoryIdentifier {
  type: string;
  value: string;
}

/** A package an advisory's vulnerability names, with its affected range. */
interface AdvisoryVulnerability {
  package: { ecosystem: string; name: string } | null;
  vulnerable_version_range: string | null;
  first_patched_version: string | null;
  vulnerable_functions: string[] | null;
}

/** A CWE the advisory is classified under. */
interface AdvisoryCWE {
  cwe_id: string;
  name: string;
}

/** The global-advisory shape /api/v3/advisories serves. */
interface GlobalAdvisory {
  ghsa_id: string;
  cve_id: string | null;
  summary: string;
  description: string | null;
  severity: string;
  html_url: string;
  published_at: string;
  updated_at: string;
  withdrawn_at: string | null;
  identifiers: AdvisoryIdentifier[];
  references: string[] | null;
  vulnerabilities: AdvisoryVulnerability[] | null;
  cwes: AdvisoryCWE[] | null;
  cvss: { score: number | null; vector_string: string | null } | null;
  source_code_location: string | null;
}

/** Severity values the database filter offers, in GitHub's own order. */
const SEVERITIES = ["critical", "high", "medium", "low"] as const;

/**
 * The ecosystems the advisory database can be filtered by. This is the
 * SecurityAdvisoryEcosystem enum, which is also the set the server's
 * version-comparison knows how to reason about.
 */
const ECOSYSTEMS = [
  "npm",
  "pip",
  "maven",
  "nuget",
  "rubygems",
  "go",
  "composer",
  "rust",
  "actions",
  "pub",
  "erlang",
  "swift",
] as const;

const fetchGlobalAdvisories = (params: URLSearchParams) =>
  ghFetch<GlobalAdvisory[]>(`/api/v3/advisories?${params.toString()}`);

const fetchGlobalAdvisory = (ghsaId: string) =>
  ghFetch<GlobalAdvisory>(`/api/v3/advisories/${enc(ghsaId)}`);

/** severityStyle maps a severity onto the palette's status colors. */
function severityStyle(severity: string): { background: string; color: string } {
  switch (severity) {
    case "critical":
      return { background: "var(--color-danger-subtle)", color: "var(--color-danger-fg)" };
    case "high":
      return { background: "var(--color-severe-subtle)", color: "var(--color-severe-fg)" };
    case "medium":
      return { background: "var(--color-attention-subtle)", color: "var(--color-attention-fg)" };
    default:
      return { background: "var(--color-neutral-subtle)", color: "var(--color-fg-muted)" };
  }
}

/** A severity pill. The severity word is the label — colour alone must not
 *  carry the meaning, which is why the text is never dropped. */
function SeverityBadge({ severity }: { severity: string }) {
  const style = severityStyle(severity);
  return (
    <span
      className="inline-block rounded-full px-2 py-0.5"
      style={{ ...style, fontSize: "0.75rem", fontWeight: 600, lineHeight: 1.5 }}
    >
      {severity}
    </span>
  );
}

/** The advisory database listing at /ui/advisories. */
export function GlobalAdvisoriesPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const severity = searchParams.get("severity") ?? "";
  const ecosystem = searchParams.get("ecosystem") ?? "";
  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const search = searchParams.get("q") ?? "";

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(searchParams);
    if (value) next.set(key, value);
    else next.delete(key);
    setSearchParams(next, { replace: true });
  };

  const requestParams = new URLSearchParams();
  if (severity) requestParams.set("severity", severity);
  if (ecosystem) requestParams.set("ecosystem", ecosystem);

  const {
    data: advisories = [],
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["global-advisories", severity, ecosystem],
    queryFn: () => fetchGlobalAdvisories(requestParams),
  });

  // The summary/identifier search is applied here rather than sent to the
  // server: /advisories has no free-text query parameter, and inventing one
  // would be a route this instance answers and GitHub does not.
  const needle = search.trim().toLowerCase();
  const shown = needle
    ? advisories.filter(
        (advisory) =>
          advisory.summary.toLowerCase().includes(needle) ||
          advisory.ghsa_id.toLowerCase().includes(needle) ||
          (advisory.cve_id ?? "").toLowerCase().includes(needle) ||
          (advisory.vulnerabilities ?? []).some((vulnerability) =>
            (vulnerability.package?.name ?? "").toLowerCase().includes(needle),
          ),
      )
    : advisories;

  return (
    <div>
      <PageTitle
        icon={<LockIcon size={20} />}
        title="Advisory database"
        meta="Security advisories published on this instance. Anyone can read these; drafts stay private to the repository that filed them."
      />

      <form
        className="mb-4 flex flex-wrap items-end gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          setParam("q", query);
        }}
      >
        <div className="flex flex-col gap-1">
          <label htmlFor="advisory-search" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
            Search
          </label>
          <input
            id="advisory-search"
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="summary, GHSA, CVE or package"
            style={{ fontSize: "0.85rem", padding: "0.35rem 0.5rem", minWidth: "16rem" }}
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="advisory-severity" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
            Severity
          </label>
          <select
            id="advisory-severity"
            value={severity}
            onChange={(event) => setParam("severity", event.target.value)}
            style={{ fontSize: "0.85rem", padding: "0.35rem 0.5rem" }}
          >
            <option value="">Any severity</option>
            {SEVERITIES.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="advisory-ecosystem" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
            Ecosystem
          </label>
          <select
            id="advisory-ecosystem"
            value={ecosystem}
            onChange={(event) => setParam("ecosystem", event.target.value)}
            style={{ fontSize: "0.85rem", padding: "0.35rem 0.5rem" }}
          >
            <option value="">Any ecosystem</option>
            {ECOSYSTEMS.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </div>
        <Button type="submit" size="sm">
          Search
        </Button>
      </form>

      {isLoading ? (
        <Spinner label="loading the advisory database" />
      ) : isError ? (
        <InlineError title="Failed to load the advisory database" detail={String(error)} />
      ) : shown.length === 0 ? (
        <Blankslate icon={<LockIcon size={24} />} title="No advisories match">
          This instance has published no security advisory matching these filters. Advisories appear here once a
          repository publishes one it drafted.
        </Blankslate>
      ) : (
        <Box>
          <ul className="divide-y" style={{ borderColor: "var(--color-border)" }}>
            {shown.map((advisory) => (
              <li key={advisory.ghsa_id} className="px-3 py-3">
                <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                  <SeverityBadge severity={advisory.severity} />
                  <Link
                    to={`/ui/advisories/${enc(advisory.ghsa_id)}`}
                    className="min-w-0 break-words"
                    style={{ fontWeight: 600, lineHeight: 1.625 }}
                  >
                    {advisory.summary}
                  </Link>
                </div>
                <div className="mt-1 flex flex-wrap gap-x-3" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                  <span>{advisory.ghsa_id}</span>
                  {advisory.cve_id && <span>{advisory.cve_id}</span>}
                  {(advisory.vulnerabilities ?? []).map(
                    (vulnerability, index) =>
                      vulnerability.package && (
                        <span key={`${vulnerability.package.name}-${index}`}>
                          {vulnerability.package.ecosystem}/{vulnerability.package.name}
                        </span>
                      ),
                  )}
                  <span>
                    published <RelativeTime iso={advisory.published_at} />
                  </span>
                  {advisory.withdrawn_at && <span>withdrawn</span>}
                </div>
              </li>
            ))}
          </ul>
        </Box>
      )}
    </div>
  );
}

/** The advisory detail view at /ui/advisories/:ghsaId. */
export function GlobalAdvisoryDetailPage() {
  const { ghsaId = "" } = useParams<{ ghsaId: string }>();
  const {
    data: advisory,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["global-advisory", ghsaId],
    queryFn: () => fetchGlobalAdvisory(ghsaId),
    enabled: !!ghsaId,
  });

  if (isLoading) return <Spinner label={`loading advisory ${ghsaId}`} />;
  if (isError) return <InlineError title="Failed to load the advisory" detail={String(error)} />;
  if (!advisory) return <InlineError title="Advisory not found" detail={ghsaId} />;

  const vulnerabilities = advisory.vulnerabilities ?? [];
  const cwes = advisory.cwes ?? [];
  const references = advisory.references ?? [];

  return (
    <div>
      <PageTitle
        icon={<LockIcon size={20} />}
        title={advisory.summary}
        meta={
          <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <SeverityBadge severity={advisory.severity} />
            <span>{advisory.ghsa_id}</span>
            {advisory.cve_id && <span>{advisory.cve_id}</span>}
            <span>
              published <RelativeTime iso={advisory.published_at} />
            </span>
            {advisory.withdrawn_at && (
              <span style={{ color: "var(--color-danger-fg)" }}>
                withdrawn <RelativeTime iso={advisory.withdrawn_at} />
              </span>
            )}
          </span>
        }
        actions={
          <ButtonLink size="sm" to="/ui/advisories">
            All advisories
          </ButtonLink>
        }
      />

      {advisory.withdrawn_at && (
        <Box className="mb-4">
          <div className="px-3 py-2" style={{ fontSize: "0.85rem", color: "var(--color-danger-fg)" }}>
            This advisory has been withdrawn. It is kept addressable so a client that already recorded it learns that
            it no longer stands.
          </div>
        </Box>
      )}

      <div className="grid gap-4 md:grid-cols-2">
        <Box>
          <h2 className="px-3 pt-3" style={{ fontSize: "0.95rem", fontWeight: 600 }}>
            Description
          </h2>
          <p className="px-3 pb-3 pt-2" style={{ fontSize: "0.875rem", whiteSpace: "pre-wrap" }}>
            {advisory.description || "This advisory carries no long description."}
          </p>
        </Box>

        <Box>
          <h2 className="px-3 pt-3" style={{ fontSize: "0.95rem", fontWeight: 600 }}>
            Affected packages
          </h2>
          {vulnerabilities.length === 0 ? (
            <p className="px-3 pb-3 pt-2" style={{ fontSize: "0.875rem", color: "var(--color-fg-muted)" }}>
              This advisory names no package coordinates.
            </p>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full" style={{ fontSize: "0.85rem" }}>
                <thead>
                  <tr style={{ color: "var(--color-fg-muted)" }}>
                    <th className="px-3 py-2 text-left">Package</th>
                    <th className="px-3 py-2 text-left">Affected versions</th>
                    <th className="px-3 py-2 text-left">Patched in</th>
                  </tr>
                </thead>
                <tbody>
                  {vulnerabilities.map((vulnerability, index) => (
                    <tr key={index} className="border-t" style={{ borderColor: "var(--color-border)" }}>
                      <td className="px-3 py-2">
                        {vulnerability.package
                          ? `${vulnerability.package.ecosystem}/${vulnerability.package.name}`
                          : "—"}
                      </td>
                      <td className="px-3 py-2">{vulnerability.vulnerable_version_range || "—"}</td>
                      <td className="px-3 py-2">{vulnerability.first_patched_version || "not yet patched"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Box>

        <Box>
          <h2 className="px-3 pt-3" style={{ fontSize: "0.95rem", fontWeight: 600 }}>
            Severity
          </h2>
          <dl className="px-3 pb-3 pt-2" style={{ fontSize: "0.85rem" }}>
            <div className="flex gap-2">
              <dt style={{ color: "var(--color-fg-muted)" }}>CVSS score</dt>
              <dd>{advisory.cvss?.score ?? "not scored"}</dd>
            </div>
            {advisory.cvss?.vector_string && (
              <div className="mt-1 flex gap-2">
                <dt style={{ color: "var(--color-fg-muted)" }}>Vector</dt>
                <dd className="break-all">{advisory.cvss.vector_string}</dd>
              </div>
            )}
            <div className="mt-1 flex gap-2">
              <dt style={{ color: "var(--color-fg-muted)" }}>Weaknesses</dt>
              <dd>{cwes.length === 0 ? "none recorded" : cwes.map((cwe) => cwe.cwe_id).join(", ")}</dd>
            </div>
          </dl>
        </Box>

        <Box>
          <h2 className="px-3 pt-3" style={{ fontSize: "0.95rem", fontWeight: 600 }}>
            Identifiers and references
          </h2>
          <ul className="px-3 pb-3 pt-2" style={{ fontSize: "0.85rem" }}>
            {advisory.identifiers.map((identifier) => (
              <li key={`${identifier.type}-${identifier.value}`}>
                {identifier.type}: {identifier.value}
              </li>
            ))}
            {references.map((reference) => (
              <li key={reference} className="break-all">
                <a href={reference} rel="noreferrer noopener">
                  {reference}
                </a>
              </li>
            ))}
            {advisory.source_code_location && (
              <li className="mt-1 break-all">
                Source: <a href={advisory.source_code_location}>{advisory.source_code_location}</a>
              </li>
            )}
          </ul>
        </Box>
      </div>
    </div>
  );
}
