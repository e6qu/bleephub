import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchSecurityAdvisories,
  fetchSecurityAdvisory,
  createSecurityAdvisory,
  createSecurityAdvisoryTemporaryFork,
  updateSecurityAdvisory,
  requestCVE,
  reportVulnerability,
} from "../api.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { RepoHeader } from "../components/PageHeader.js";
import { Box, Button, Modal, FormLabel, ErrorBanner, DialogActions } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { RelativeTime } from "../components/RelativeTime.js";
import type {
  GithubSecurityAdvisory,
  GithubSecurityAdvisorySeverity,
  GithubSecurityAdvisoryState,
  GithubSecurityAdvisoryCreatePayload,
  GithubSecurityAdvisoryUpdatePayload,
  GithubVulnerabilityReportPayload,
} from "../types.js";

type SeverityFilter = "all" | GithubSecurityAdvisorySeverity;

const SEVERITIES: GithubSecurityAdvisorySeverity[] = ["critical", "high", "medium", "low"];

// ─── Store-backed extras beyond the base advisory payloads ─────────────────────────
// The server's CreateAdvisoryReq (create + report) also persists cvss_score,
// cvss_vector, and a vulnerabilities[] product list; the update handler
// accepts cvss_score/cvss_vector (but not vulnerabilities). Credits are NOT
// stored (the API always returns empty credits arrays), so no credits UI.

interface AdvisoryVulnerabilityPayload {
  package: { ecosystem: string; name: string };
  vulnerable_version_range: string;
  first_patched_version: string;
}

type AdvisoryCreateExtras = GithubSecurityAdvisoryCreatePayload & {
  cvss_score?: number;
  cvss_vector?: string;
  vulnerabilities?: AdvisoryVulnerabilityPayload[];
};

type AdvisoryUpdateExtras = GithubSecurityAdvisoryUpdatePayload & {
  cvss_score?: number;
  cvss_vector?: string;
};

/** Response-side fields the base display type does not declare. */
type AdvisoryWithExtras = GithubSecurityAdvisory & {
  cvss?: { vector_string: string | null; score: number | null };
  vulnerabilities?: {
    package: { ecosystem: string; name: string } | null;
    vulnerable_version_range: string | null;
    patched_versions: string | null;
  }[];
};

/** One editable affected-product row in the create/report form. */
interface VulnerabilityRow {
  ecosystem: string;
  name: string;
  range: string;
  patched: string;
}

const ECOSYSTEMS = [
  "npm", "pip", "rubygems", "maven", "nuget", "composer", "go", "rust",
  "erlang", "actions", "pub", "swift", "other",
];

const STATE_LABELS: Record<string, string> = {
  triage: "Triage",
  draft: "Draft",
  published: "Published",
  closed: "Closed",
  withdrawn: "Withdrawn",
};

export function SecurityAdvisoriesPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const [severityFilter, setSeverityFilter] = useState<SeverityFilter>("all");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [showReport, setShowReport] = useState(false);
  const [showEdit, setShowEdit] = useState(false);
  const counts = useOpenCounts(owner, repo);
  const queryClient = useQueryClient();

  const {
    data: advisories = [],
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["security-advisories", owner, repo],
    queryFn: () => fetchSecurityAdvisories(owner, repo),
    enabled: !!owner && !!repo,
  });

  const selectedQuery = useQuery({
    queryKey: ["security-advisory", owner, repo, selectedId],
    queryFn: () => fetchSecurityAdvisory(owner, repo, selectedId!),
    enabled: selectedId !== null,
  });

  const createMutation = useMutation({
    mutationFn: (payload: AdvisoryCreateExtras) =>
      createSecurityAdvisory(owner, repo, payload),
    onSuccess: (created) => {
      queryClient.invalidateQueries({ queryKey: ["security-advisories", owner, repo] });
      setSelectedId(created.ghsa_id);
      setShowCreate(false);
    },
  });

  const reportMutation = useMutation({
    mutationFn: (payload: GithubVulnerabilityReportPayload & AdvisoryCreateExtras) =>
      reportVulnerability(owner, repo, payload),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["security-advisories", owner, repo] });
      setShowReport(false);
    },
  });

  const cveMutation = useMutation({
    mutationFn: (ghsaId: string) => requestCVE(owner, repo, ghsaId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["security-advisories", owner, repo] });
      if (selectedId) {
        queryClient.invalidateQueries({ queryKey: ["security-advisory", owner, repo, selectedId] });
      }
    },
  });

  const updateMutation = useMutation({
    mutationFn: (payload: AdvisoryUpdateExtras) =>
      updateSecurityAdvisory(owner, repo, selectedId!, payload),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["security-advisories", owner, repo] });
      queryClient.invalidateQueries({ queryKey: ["security-advisory", owner, repo, selectedId] });
      setShowEdit(false);
    },
  });

  const forkMutation = useMutation({
    mutationFn: (ghsaId: string) =>
      createSecurityAdvisoryTemporaryFork(owner, repo, ghsaId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["security-advisories", owner, repo] });
      queryClient.invalidateQueries({ queryKey: ["security-advisory", owner, repo, selectedId] });
    },
  });

  useEffect(() => {
    setSelectedId(null);
    setShowEdit(false);
  }, [owner, repo, severityFilter]);

  const filtered =
    severityFilter === "all"
      ? advisories
      : advisories.filter((a) => a.severity === severityFilter);

  if (isLoading) return <Spinner label={`loading ${owner}/${repo} security advisories`} />;
  if (isError) return <InlineError title="Failed to load security advisories" detail={String(error)} />;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="security" {...counts} />
      <MutationError of={updateMutation} />
      <MutationError of={forkMutation} />

      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <label style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>Severity:</label>
          <select
            aria-label="Filter by severity"
            value={severityFilter}
            onChange={(e) => setSeverityFilter(e.target.value as SeverityFilter)}
            style={{ fontSize: "0.85rem", padding: "0.35rem 0.5rem" }}
          >
            <option value="all">All</option>
            {SEVERITIES.map((s) => (
              <option key={s} value={s}>
                {s.charAt(0).toUpperCase() + s.slice(1)}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="secondary" size="sm" onClick={() => setShowReport(true)}>
            Report vulnerability
          </Button>
          <Button variant="primary" size="sm" onClick={() => setShowCreate(true)}>
            New advisory
          </Button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <Box>
          <h3 style={{ marginTop: 0, marginBottom: "0.75rem" }}>Advisories ({filtered.length})</h3>
          {filtered.length === 0 ? (
            <p style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No security advisories.</p>
          ) : (
            <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
              {filtered.map((advisory) => (
                <li key={advisory.ghsa_id} style={{ borderBottom: "1px solid var(--color-border)" }}>
                  <button
                    type="button"
                    aria-pressed={selectedId === advisory.ghsa_id}
                    onClick={() => setSelectedId(advisory.ghsa_id)}
                    style={{
                      display: "block",
                      width: "100%",
                      padding: "0.6rem 0.4rem",
                      border: 0,
                      textAlign: "left",
                      cursor: "pointer",
                      color: "inherit",
                      background: selectedId === advisory.ghsa_id ? "var(--color-accent-subtle)" : "transparent",
                    }}
                  >
                    <div style={{ fontWeight: 600, fontSize: "0.9rem" }}>{advisory.summary}</div>
                    <div style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                      {advisory.ghsa_id}
                      {advisory.cve_id ? ` / ${advisory.cve_id}` : ""}
                      {" · "}
                      {STATE_LABELS[advisory.state] ?? advisory.state}
                      {" · "}
                      {advisory.severity}
                    </div>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </Box>

        <Box>
          {selectedQuery.isLoading ? (
            <Spinner label={`loading advisory ${selectedId}`} />
          ) : selectedQuery.isError ? (
            <InlineError title="Failed to load security advisory" detail={String(selectedQuery.error)} />
          ) : selectedQuery.data ? (
            <AdvisoryDetail
              advisory={selectedQuery.data}
              onEdit={() => setShowEdit(true)}
              onChangeState={(state) => updateMutation.mutate({ state })}
              statePending={updateMutation.isPending}
              onCreateFork={() => forkMutation.mutate(selectedQuery.data.ghsa_id)}
              forkPending={forkMutation.isPending}
              onRequestCVE={() => cveMutation.mutate(selectedQuery.data.ghsa_id)}
              cvePending={cveMutation.isPending}
              cveError={cveMutation.error}
            />
          ) : (
            <p style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>Select an advisory to view details.</p>
          )}
        </Box>
      </div>

      {showCreate && (
        <AdvisoryFormModal
          title="Create security advisory"
          onClose={() => setShowCreate(false)}
          onSubmit={(payload) => createMutation.mutate(payload)}
          pending={createMutation.isPending}
          error={createMutation.error}
        />
      )}

      {showReport && (
        <AdvisoryFormModal
          title="Report vulnerability"
          onClose={() => setShowReport(false)}
          onSubmit={(payload) => reportMutation.mutate(payload)}
          pending={reportMutation.isPending}
          error={reportMutation.error}
        />
      )}

      {showEdit && selectedQuery.data && (
        <AdvisoryEditModal
          advisory={selectedQuery.data}
          onClose={() => setShowEdit(false)}
          onSubmit={(payload) => updateMutation.mutate(payload)}
          pending={updateMutation.isPending}
          error={updateMutation.error}
        />
      )}
    </div>
  );
}

function AdvisoryDetail({
  advisory,
  onEdit,
  onChangeState,
  statePending,
  onCreateFork,
  forkPending,
  onRequestCVE,
  cvePending,
  cveError,
}: {
  advisory: GithubSecurityAdvisory;
  onEdit: () => void;
  onChangeState: (state: GithubSecurityAdvisoryState) => void;
  statePending: boolean;
  onCreateFork: () => void;
  forkPending: boolean;
  onRequestCVE: () => void;
  cvePending: boolean;
  cveError: Error | null;
}) {
  return (
    <div>
      <h3 style={{ marginTop: 0, marginBottom: "0.75rem" }}>{advisory.summary}</h3>
      <div style={{ fontSize: "0.85rem", marginBottom: "0.75rem" }}>
        <div>
          <strong>GHSA:</strong> {advisory.ghsa_id}
        </div>
        {advisory.cve_id && (
          <div>
            <strong>CVE:</strong> {advisory.cve_id}
          </div>
        )}
        <div>
          <strong>State:</strong> {STATE_LABELS[advisory.state] ?? advisory.state}
        </div>
        <div>
          <strong>Severity:</strong> {advisory.severity}
        </div>
        {advisory.cwe_ids && advisory.cwe_ids.length > 0 && (
          <div>
            <strong>CWEs:</strong> {advisory.cwe_ids.join(", ")}
          </div>
        )}
        {(() => {
          const extras = advisory as AdvisoryWithExtras;
          const cvss = extras.cvss;
          const products = (extras.vulnerabilities ?? []).filter((v) => v.package || v.vulnerable_version_range);
          return (
            <>
              {cvss && (cvss.score != null || cvss.vector_string) && (
                <div>
                  <strong>CVSS:</strong>{" "}
                  {cvss.score != null ? cvss.score : "—"}
                  {cvss.vector_string ? ` (${cvss.vector_string})` : ""}
                </div>
              )}
              {products.length > 0 && (
                <div>
                  <strong>Affected products:</strong>
                  <ul style={{ margin: "0.25rem 0 0", paddingLeft: "1.2rem" }}>
                    {products.map((v, i) => (
                      <li key={i}>
                        {v.package ? `${v.package.ecosystem} / ${v.package.name}` : "unspecified package"}
                        {v.vulnerable_version_range ? ` · affected ${v.vulnerable_version_range}` : ""}
                        {v.patched_versions ? ` · patched in ${v.patched_versions}` : ""}
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </>
          );
        })()}
        <div>
          <strong>Created:</strong> <RelativeTime iso={advisory.created_at} />
        </div>
        <div>
          <strong>Updated:</strong> <RelativeTime iso={advisory.updated_at} />
        </div>
        {advisory.published_at && (
          <div>
            <strong>Published:</strong> <RelativeTime iso={advisory.published_at} />
          </div>
        )}
        <div style={{ marginTop: "0.5rem", whiteSpace: "pre-wrap" }}>
          {advisory.description || "No description provided."}
        </div>
        {advisory.private_fork && (
          <div style={{ marginTop: "0.5rem" }}>
            <strong>Temporary private fork:</strong>{" "}
            <Link
              to={`/ui/repos/${advisory.private_fork.full_name}`}
              style={{ color: "var(--color-accent)" }}
            >
              {advisory.private_fork.full_name}
            </Link>
          </div>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-2" style={{ marginBottom: "1rem" }}>
        <Button variant="secondary" size="sm" onClick={onEdit} disabled={statePending}>
          Edit advisory
        </Button>
        {advisory.state === "draft" && (
          <Button variant="primary" size="sm" onClick={() => onChangeState("published")} disabled={statePending}>
            {statePending ? "Publishing…" : "Publish advisory"}
          </Button>
        )}
        {advisory.state === "triage" && (
          <Button variant="primary" size="sm" onClick={() => onChangeState("draft")} disabled={statePending}>
            {statePending ? "Moving…" : "Move to draft"}
          </Button>
        )}
        {(advisory.state === "triage" ||
          advisory.state === "draft" ||
          advisory.state === "published") && (
          <Button variant="danger" size="sm" onClick={() => onChangeState("closed")} disabled={statePending}>
            {statePending ? "Closing…" : "Close advisory"}
          </Button>
        )}
        {advisory.state === "closed" && (
          <Button variant="secondary" size="sm" onClick={() => onChangeState("draft")} disabled={statePending}>
            {statePending ? "Reopening…" : "Reopen as draft"}
          </Button>
        )}
        {!advisory.private_fork && (advisory.state === "triage" || advisory.state === "draft") && (
          <Button variant="secondary" size="sm" onClick={onCreateFork} disabled={forkPending}>
            {forkPending ? "Creating fork…" : "Create temporary private fork"}
          </Button>
        )}
        {!advisory.cve_id &&
          (advisory.state === "triage" ||
            advisory.state === "draft" ||
            advisory.state === "published") && (
          <Button variant="secondary" size="sm" onClick={onRequestCVE} disabled={cvePending}>
            {cvePending ? "Requesting CVE…" : "Request CVE"}
          </Button>
        )}
      </div>

      {cveError && (
        <div style={{ color: "var(--color-status-error)", fontSize: "0.85rem" }}>
          {cveError instanceof Error ? cveError.message : String(cveError)}
        </div>
      )}
    </div>
  );
}

function AdvisoryEditModal({
  advisory,
  onClose,
  onSubmit,
  pending,
  error,
}: {
  advisory: GithubSecurityAdvisory;
  onClose: () => void;
  onSubmit: (payload: AdvisoryUpdateExtras) => void;
  pending: boolean;
  error: Error | null;
}) {
  const extras = advisory as AdvisoryWithExtras;
  const [summary, setSummary] = useState(advisory.summary);
  const [description, setDescription] = useState(advisory.description ?? "");
  const [severity, setSeverity] = useState(advisory.severity);
  const [cwe, setCwe] = useState((advisory.cwe_ids ?? []).join(", "));
  const [cvssScore, setCvssScore] = useState(extras.cvss?.score != null ? String(extras.cvss.score) : "");
  const [cvssVector, setCvssVector] = useState(extras.cvss?.vector_string ?? "");
  const [validationError, setValidationError] = useState<string | null>(null);

  const handleSubmit = () => {
    setValidationError(null);
    if (!summary.trim()) {
      setValidationError("Summary is required.");
      return;
    }
    if (!description.trim()) {
      setValidationError("Description is required.");
      return;
    }
    if (cvssScore.trim() && (Number.isNaN(Number(cvssScore)) || Number(cvssScore) < 0 || Number(cvssScore) > 10)) {
      setValidationError("CVSS score must be a number between 0 and 10.");
      return;
    }
    onSubmit({
      summary: summary.trim(),
      description: description.trim(),
      severity,
      cwe_ids: cwe
        .split(",")
        .map((value) => value.trim())
        .filter(Boolean),
      ...(cvssScore.trim() ? { cvss_score: Number(cvssScore) } : {}),
      ...(cvssVector.trim() ? { cvss_vector: cvssVector.trim() } : {}),
    });
  };

  return (
    <Modal title={`Edit ${advisory.ghsa_id}`} onClose={onClose}>
      <FormLabel id="edit-advisory-summary">Summary</FormLabel>
      <input
        id="edit-advisory-summary"
        type="text"
        value={summary}
        onChange={(event) => setSummary(event.target.value)}
        className="mb-4 w-full"
      />

      <FormLabel id="edit-advisory-description">Description</FormLabel>
      <textarea
        id="edit-advisory-description"
        rows={5}
        value={description}
        onChange={(event) => setDescription(event.target.value)}
        className="mb-4 w-full"
        style={{ resize: "vertical" }}
      />

      <FormLabel id="edit-advisory-severity">Severity</FormLabel>
      <select
        id="edit-advisory-severity"
        value={severity}
        onChange={(event) => setSeverity(event.target.value as GithubSecurityAdvisorySeverity)}
        className="mb-4 w-full"
      >
        {SEVERITIES.map((value) => (
          <option key={value} value={value}>
            {value.charAt(0).toUpperCase() + value.slice(1)}
          </option>
        ))}
      </select>

      <FormLabel id="edit-advisory-cwe">CWE IDs (comma separated)</FormLabel>
      <input
        id="edit-advisory-cwe"
        type="text"
        value={cwe}
        onChange={(event) => setCwe(event.target.value)}
        className="mb-4 w-full"
      />

      <div className="mb-4 flex flex-wrap gap-3">
        <div>
          <FormLabel id="edit-advisory-cvss-score">CVSS score (0–10, optional)</FormLabel>
          <input
            id="edit-advisory-cvss-score"
            type="number"
            min={0}
            max={10}
            step={0.1}
            value={cvssScore}
            onChange={(event) => setCvssScore(event.target.value)}
            className="w-32"
          />
        </div>
        <div style={{ flex: 1, minWidth: "16rem" }}>
          <FormLabel id="edit-advisory-cvss-vector">CVSS vector string (optional)</FormLabel>
          <input
            id="edit-advisory-cvss-vector"
            type="text"
            value={cvssVector}
            onChange={(event) => setCvssVector(event.target.value)}
            placeholder="CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"
            className="w-full"
          />
        </div>
      </div>

      {(validationError || error) && (
        <ErrorBanner>
          {validationError ?? (error instanceof Error ? error.message : String(error))}
        </ErrorBanner>
      )}

      <DialogActions>
        <Button onClick={onClose} disabled={pending} variant="ghost">
          Cancel
        </Button>
        <Button onClick={handleSubmit} disabled={pending} variant="primary">
          {pending ? "Saving…" : "Save changes"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function AdvisoryFormModal({
  title,
  onClose,
  onSubmit,
  pending,
  error,
}: {
  title: string;
  onClose: () => void;
  onSubmit: (payload: GithubVulnerabilityReportPayload & AdvisoryCreateExtras) => void;
  pending: boolean;
  error: Error | null;
}) {
  const [summary, setSummary] = useState("");
  const [description, setDescription] = useState("");
  const [severity, setSeverity] = useState<GithubSecurityAdvisorySeverity>("medium");
  const [cwe, setCwe] = useState("");
  const [cvssScore, setCvssScore] = useState("");
  const [cvssVector, setCvssVector] = useState("");
  const [products, setProducts] = useState<VulnerabilityRow[]>([]);
  const [validationError, setValidationError] = useState<string | null>(null);

  const setProduct = (index: number, patch: Partial<VulnerabilityRow>) =>
    setProducts((rows) => rows.map((row, i) => (i === index ? { ...row, ...patch } : row)));

  const handleSubmit = () => {
    setValidationError(null);
    if (!summary.trim()) {
      setValidationError("Summary is required.");
      return;
    }
    if (!description.trim()) {
      setValidationError("Description is required.");
      return;
    }
    if (cvssScore.trim() && (Number.isNaN(Number(cvssScore)) || Number(cvssScore) < 0 || Number(cvssScore) > 10)) {
      setValidationError("CVSS score must be a number between 0 and 10.");
      return;
    }
    const vulnerabilities: AdvisoryVulnerabilityPayload[] = products
      .filter((row) => row.name.trim())
      .map((row) => ({
        package: { ecosystem: row.ecosystem, name: row.name.trim() },
        vulnerable_version_range: row.range.trim(),
        first_patched_version: row.patched.trim(),
      }));
    const payload: GithubVulnerabilityReportPayload & AdvisoryCreateExtras = {
      summary: summary.trim(),
      description: description.trim(),
      severity,
    };
    const cweIds = cwe
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    if (cweIds.length > 0) {
      payload.cwe_ids = cweIds;
    }
    if (cvssScore.trim()) payload.cvss_score = Number(cvssScore);
    if (cvssVector.trim()) payload.cvss_vector = cvssVector.trim();
    if (vulnerabilities.length > 0) payload.vulnerabilities = vulnerabilities;
    onSubmit(payload);
  };

  return (
    <Modal title={title} onClose={onClose}>
      <FormLabel id="advisory-summary">Summary</FormLabel>
      <input
        id="advisory-summary"
        type="text"
        value={summary}
        onChange={(e) => setSummary(e.target.value)}
        className="mb-4 w-full"
      />

      <FormLabel id="advisory-description">Description</FormLabel>
      <textarea
        id="advisory-description"
        rows={5}
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-4 w-full"
        style={{ resize: "vertical" }}
      />

      <FormLabel id="advisory-severity">Severity</FormLabel>
      <select
        id="advisory-severity"
        value={severity}
        onChange={(e) => setSeverity(e.target.value as GithubSecurityAdvisorySeverity)}
        className="mb-4 w-full"
      >
        {SEVERITIES.map((s) => (
          <option key={s} value={s}>
            {s.charAt(0).toUpperCase() + s.slice(1)}
          </option>
        ))}
      </select>

      <FormLabel id="advisory-cwe">CWE IDs (comma separated)</FormLabel>
      <input
        id="advisory-cwe"
        type="text"
        value={cwe}
        onChange={(e) => setCwe(e.target.value)}
        placeholder="CWE-79, CWE-89"
        className="mb-4 w-full"
      />

      <div className="mb-4 flex flex-wrap gap-3">
        <div>
          <FormLabel id="advisory-cvss-score">CVSS score (0–10, optional)</FormLabel>
          <input
            id="advisory-cvss-score"
            type="number"
            min={0}
            max={10}
            step={0.1}
            value={cvssScore}
            onChange={(e) => setCvssScore(e.target.value)}
            className="w-32"
          />
        </div>
        <div style={{ flex: 1, minWidth: "16rem" }}>
          <FormLabel id="advisory-cvss-vector">CVSS vector string (optional)</FormLabel>
          <input
            id="advisory-cvss-vector"
            type="text"
            value={cvssVector}
            onChange={(e) => setCvssVector(e.target.value)}
            placeholder="CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"
            className="w-full"
          />
        </div>
      </div>

      <div className="mb-4">
        <div className="mb-1 flex items-center justify-between">
          <span style={{ fontSize: "0.82rem", fontWeight: 600 }}>Affected products</span>
          <Button
            size="sm"
            variant="secondary"
            onClick={() =>
              setProducts((rows) => [...rows, { ecosystem: ECOSYSTEMS[0]!, name: "", range: "", patched: "" }])
            }
          >
            Add affected product
          </Button>
        </div>
        {products.length === 0 ? (
          <p style={{ margin: 0, fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
            Optionally list the packages and version ranges this advisory affects.
          </p>
        ) : (
          products.map((row, i) => (
            <div
              key={i}
              className="mb-2 flex flex-wrap items-end gap-2"
              style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", padding: "0.5rem" }}
            >
              <div>
                <FormLabel id={`advisory-vuln-ecosystem-${i}`}>Ecosystem</FormLabel>
                <select
                  id={`advisory-vuln-ecosystem-${i}`}
                  value={row.ecosystem}
                  onChange={(e) => setProduct(i, { ecosystem: e.target.value })}
                >
                  {ECOSYSTEMS.map((eco) => (
                    <option key={eco} value={eco}>{eco}</option>
                  ))}
                </select>
              </div>
              <div>
                <FormLabel id={`advisory-vuln-name-${i}`}>Package name</FormLabel>
                <input
                  id={`advisory-vuln-name-${i}`}
                  type="text"
                  value={row.name}
                  onChange={(e) => setProduct(i, { name: e.target.value })}
                  className="w-40"
                />
              </div>
              <div>
                <FormLabel id={`advisory-vuln-range-${i}`}>Affected versions</FormLabel>
                <input
                  id={`advisory-vuln-range-${i}`}
                  type="text"
                  value={row.range}
                  onChange={(e) => setProduct(i, { range: e.target.value })}
                  placeholder="< 1.2.3"
                  className="w-32"
                />
              </div>
              <div>
                <FormLabel id={`advisory-vuln-patched-${i}`}>Patched version</FormLabel>
                <input
                  id={`advisory-vuln-patched-${i}`}
                  type="text"
                  value={row.patched}
                  onChange={(e) => setProduct(i, { patched: e.target.value })}
                  placeholder="1.2.3"
                  className="w-28"
                />
              </div>
              <Button
                size="sm"
                variant="ghost"
                aria-label={`Remove affected product ${i + 1}`}
                onClick={() => setProducts((rows) => rows.filter((_, j) => j !== i))}
              >
                Remove
              </Button>
            </div>
          ))
        )}
      </div>

      {(validationError || error) && <ErrorBanner>{validationError ?? (error instanceof Error ? error.message : String(error))}</ErrorBanner>}

      <DialogActions>
        <Button onClick={onClose} disabled={pending} variant="ghost">
          Cancel
        </Button>
        <Button onClick={handleSubmit} disabled={pending} variant="primary">
          {pending ? "Saving…" : "Submit"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
