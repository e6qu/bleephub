import { useParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { OrgHeader } from "../components/PageHeader.js";
import { PageTitle, Box, SectionLabel } from "../components/ui.js";
import { fetchOrgApiInsightsSummary, fetchOrgApiInsightsSubjectStats } from "../api.js";

/**
 * Org "API Insights" (GHES): trailing-30-day API-request summary plus the top
 * request subjects, over /orgs/{org}/insights/api/*.
 */
export function OrgInsightsPage() {
  const { org = "" } = useParams<{ org: string }>();
  const summary = useQuery({
    queryKey: ["org-insights-summary", org],
    queryFn: () => fetchOrgApiInsightsSummary(org),
    enabled: !!org,
  });
  const subjects = useQuery({
    queryKey: ["org-insights-subjects", org],
    queryFn: () => fetchOrgApiInsightsSubjectStats(org),
    enabled: !!org,
  });

  const rows = (subjects.data ?? [])
    .slice()
    .sort((a, b) => b.total_request_count - a.total_request_count)
    .slice(0, 20);

  return (
    <div>
      <OrgHeader org={org} active="insights" />
      <PageTitle title="Insights" meta="API request activity across the organization (last 30 days)." />
      {summary.isError ? (
        <InlineError title="Failed to load insights" detail={String(summary.error)} />
      ) : summary.isLoading ? (
        <Spinner label="loading insights" />
      ) : (
        <div className="flex flex-col gap-5" style={{ maxWidth: "52rem" }}>
          <div className="grid gap-4 sm:grid-cols-2">
            <StatBox label="Total API requests" value={summary.data?.total_request_count ?? 0} accent="var(--color-accent)" />
            <StatBox label="Rate-limited requests" value={summary.data?.rate_limited_request_count ?? 0} accent="var(--color-status-warn)" />
          </div>

          <section>
            <SectionLabel>Top request subjects</SectionLabel>
            {subjects.isLoading ? (
              <Spinner label="loading subjects" />
            ) : subjects.isError ? (
              <InlineError title="Failed to load subject stats" detail={String(subjects.error)} />
            ) : rows.length === 0 ? (
              <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>No API activity in this window yet.</p>
            ) : (
              <Box>
                <table style={{ width: "100%", borderCollapse: "collapse", fontSize: "0.85rem" }}>
                  <thead>
                    <tr style={{ textAlign: "left", color: "var(--color-fg-muted)" }}>
                      <th scope="col" style={{ padding: "0.5rem 1rem" }}>Subject</th>
                      <th scope="col" style={{ padding: "0.5rem 1rem", textAlign: "right" }}>Requests</th>
                      <th scope="col" style={{ padding: "0.5rem 1rem", textAlign: "right" }}>Rate-limited</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((s, i) => (
                      <tr key={s.subject_name + i} style={{ borderTop: "1px solid var(--color-border)" }}>
                        <td style={{ padding: "0.5rem 1rem", color: "var(--color-fg)" }}>{s.subject_name}</td>
                        <td className="tabular-nums" style={{ padding: "0.5rem 1rem", textAlign: "right" }}>{s.total_request_count}</td>
                        <td className="tabular-nums" style={{ padding: "0.5rem 1rem", textAlign: "right", color: "var(--color-fg-muted)" }}>{s.rate_limited_request_count}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </Box>
            )}
          </section>
        </div>
      )}
    </div>
  );
}

function StatBox({ label, value, accent }: { label: string; value: number; accent: string }) {
  return (
    <Box>
      <div style={{ padding: "1rem", borderLeft: `4px solid ${accent}` }}>
        <div className="tabular-nums" style={{ fontSize: "1.6rem", fontWeight: 700 }}>{value.toLocaleString()}</div>
        <div style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>{label}</div>
      </div>
    </Box>
  );
}
