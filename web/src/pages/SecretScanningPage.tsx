import { useEffect, useState } from "react";
import { useParams } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  fetchSecretScanningAlerts,
  fetchSecretScanningAlert,
  fetchSecretScanningAlertLocations,
  updateSecretScanningAlert,
} from "../api.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { RepoHeader } from "../components/Shell.js";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { Box, Button } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import type {
  GithubSecretScanningAlert,
  GithubSecretScanningLocation,
  GithubSecretScanningResolution,
} from "../types.js";

type FilterState = "all" | "open" | "resolved";

const RESOLUTIONS: { value: GithubSecretScanningResolution; label: string }[] = [
  { value: "false_positive", label: "False positive" },
  { value: "wont_fix", label: "Won't fix" },
  { value: "revoked", label: "Revoked" },
  { value: "used_in_tests", label: "Used in tests" },
  { value: "pattern_deleted", label: "Pattern deleted" },
  { value: "pattern_edited", label: "Pattern edited" },
];

export function SecretScanningPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const [filter, setFilter] = useState<FilterState>("all");
  const [selectedNumber, setSelectedNumber] = useState<number | null>(null);
  const counts = useOpenCounts(owner, repo);
  const queryClient = useQueryClient();

  const filters = { state: filter === "all" ? undefined : filter };
  const {
    data: alerts = [],
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["secret-scanning", owner, repo, filter],
    queryFn: () => fetchSecretScanningAlerts(owner, repo, filters),
    enabled: !!owner && !!repo,
  });

  const selectedQuery = useQuery({
    queryKey: ["secret-scanning-alert", owner, repo, selectedNumber],
    queryFn: () => fetchSecretScanningAlert(owner, repo, selectedNumber!),
    enabled: selectedNumber !== null,
  });

  const locationsQuery = useQuery({
    queryKey: ["secret-scanning-locations", owner, repo, selectedNumber],
    queryFn: () => fetchSecretScanningAlertLocations(owner, repo, selectedNumber!),
    enabled: selectedNumber !== null,
  });

  const resolveMutation = useMutation({
    mutationFn: (payload: {
      number: number;
      state: "open" | "resolved";
      resolution?: GithubSecretScanningResolution;
      resolution_comment?: string;
    }) => updateSecretScanningAlert(owner, repo, payload.number, payload),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["secret-scanning", owner, repo] });
      if (selectedNumber !== null) {
        queryClient.invalidateQueries({ queryKey: ["secret-scanning-alert", owner, repo, selectedNumber] });
        queryClient.invalidateQueries({ queryKey: ["secret-scanning-locations", owner, repo, selectedNumber] });
      }
    },
  });

  useEffect(() => {
    setSelectedNumber(null);
  }, [owner, repo, filter]);

  if (isLoading) return <Spinner label={`loading ${owner}/${repo} secret scanning`} />;
  if (isError) return <InlineError title="Failed to load secret scanning alerts" detail={String(error)} />;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="security" {...counts} />

      <MutationError of={resolveMutation} />

      <div className="mb-4 flex flex-wrap items-center gap-2">
        <label style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>State:</label>
        <select
          aria-label="State"
          value={filter}
          onChange={(e) => setFilter(e.target.value as FilterState)}
          style={{ fontSize: "0.85rem", padding: "0.35rem 0.5rem" }}
        >
          <option value="all">All</option>
          <option value="open">Open</option>
          <option value="resolved">Resolved</option>
        </select>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <Box>
          <h3 style={{ marginTop: 0, marginBottom: "0.75rem" }}>Alerts ({alerts.length})</h3>
          {alerts.length === 0 ? (
            <p style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No secret scanning alerts.</p>
          ) : (
            <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
              {alerts.map((alert) => (
                <li key={alert.number} style={{ borderBottom: "1px solid var(--color-border)" }}>
                  <button
                    type="button"
                    aria-pressed={selectedNumber === alert.number}
                    onClick={() => setSelectedNumber(alert.number)}
                    style={{
                      display: "block",
                      width: "100%",
                      padding: "0.6rem 0.4rem",
                      border: 0,
                      textAlign: "left",
                      cursor: "pointer",
                      color: "inherit",
                      background: selectedNumber === alert.number ? "var(--color-accent-subtle)" : "transparent",
                    }}
                  >
                    <div style={{ fontWeight: 600, fontSize: "0.9rem" }}>
                      #{alert.number} {alert.secret_type_display_name}
                    </div>
                    <div style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                      {alert.state}
                      {alert.resolution ? ` — ${alert.resolution}` : ""}
                    </div>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </Box>

        <Box>
          {selectedQuery.isLoading ? (
            <Spinner label={`loading alert ${selectedNumber}`} />
          ) : selectedQuery.isError ? (
            <InlineError title="Failed to load secret scanning alert" detail={String(selectedQuery.error)} />
          ) : selectedQuery.data ? (
            <AlertDetail
              alert={selectedQuery.data}
              locations={locationsQuery.data ?? []}
              locationsLoading={locationsQuery.isLoading}
              locationsError={locationsQuery.error}
              onResolve={(resolution, comment) =>
                resolveMutation.mutate({
                  number: selectedQuery.data.number,
                  state: "resolved",
                  resolution,
                  resolution_comment: comment,
                })
              }
              onReopen={() => resolveMutation.mutate({ number: selectedQuery.data.number, state: "open" })}
            />
          ) : (
            <p style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>Select an alert to view details.</p>
          )}
        </Box>
      </div>
    </div>
  );
}

function AlertDetail({
  alert,
  locations,
  locationsLoading,
  locationsError,
  onResolve,
  onReopen,
}: {
  alert: GithubSecretScanningAlert;
  locations: GithubSecretScanningLocation[];
  locationsLoading: boolean;
  locationsError: Error | null;
  onResolve: (resolution: GithubSecretScanningResolution, comment: string) => void;
  onReopen: () => void;
}) {
  const [resolution, setResolution] = useState<GithubSecretScanningResolution>("false_positive");
  const [comment, setComment] = useState("");

  return (
    <div>
      <h3 style={{ marginTop: 0, marginBottom: "0.75rem" }}>Alert #{alert.number}</h3>
      <div style={{ fontSize: "0.85rem", marginBottom: "0.75rem" }}>
        <div>
          <strong>Type:</strong> {alert.secret_type_display_name}
        </div>
        <div>
          <strong>State:</strong> {alert.state}
        </div>
        {alert.resolution && (
          <div>
            <strong>Resolution:</strong> {alert.resolution}
          </div>
        )}
        <div>
          <strong>Created:</strong> {new Date(alert.created_at).toLocaleString()}
        </div>
      </div>

      {alert.state === "open" ? (
        <div style={{ marginBottom: "1rem" }}>
          <label style={{ fontSize: "0.85rem" }}>Resolution</label>
          <select
            value={resolution}
            onChange={(e) => setResolution(e.target.value as GithubSecretScanningResolution)}
            style={{ fontSize: "0.85rem", padding: "0.35rem 0.5rem", display: "block", marginBottom: "0.5rem" }}
          >
            {RESOLUTIONS.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </select>
          <input
            type="text"
            placeholder="Comment (optional)"
            value={comment}
            onChange={(e) => setComment(e.target.value)}
            style={{ fontSize: "0.85rem", padding: "0.35rem 0.5rem", width: "100%", marginBottom: "0.5rem" }}
          />
          <Button type="button" onClick={() => onResolve(resolution, comment)} size="sm">
            Resolve
          </Button>
        </div>
      ) : (
        <div style={{ marginBottom: "1rem" }}>
          <Button type="button" onClick={onReopen} size="sm">
            Reopen
          </Button>
        </div>
      )}

      <h4 style={{ fontSize: "0.9rem", marginBottom: "0.5rem" }}>Locations ({locations.length})</h4>
      {locationsLoading ? (
        <Spinner label={`loading locations for alert ${alert.number}`} />
      ) : locationsError ? (
        <InlineError title="Failed to load alert locations" detail={String(locationsError)} />
      ) : locations.length === 0 ? (
        <p style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No locations.</p>
      ) : (
        <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
          {locations.map((loc, idx) => (
            <li key={idx} style={{ fontSize: "0.85rem", padding: "0.4rem 0", borderBottom: "1px solid var(--color-border)" }}>
              <div>
                <strong>{loc.details.path}</strong>
              </div>
              <div style={{ color: "var(--color-fg-muted)" }}>
                lines {loc.details.start_line}–{loc.details.end_line}, columns {loc.details.start_column}–
                {loc.details.end_column}
              </div>
              <div style={{ color: "var(--color-fg-muted)" }}>commit {loc.details.commit_sha.slice(0, 7)}</div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
