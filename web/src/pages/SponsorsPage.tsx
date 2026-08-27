import { useState } from "react";
import { Link, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { ghFetch, ghPostJSON, ghSend } from "../api.js";
import {
  Blankslate,
  Box,
  Button,
  ErrorBanner,
  FormLabel,
  SectionLabel,
  StatCard,
  StateLabel,
} from "../components/ui.js";
import { confirmAction } from "../components/confirmAction.js";

// Fetch wrappers and this icon live in the lazy page, not the shared modules
// reachable from the entry chunk, to protect the entry bundle budget.
const enc = encodeURIComponent;

function HeartIcon({ size = 16 }: { size?: number }) {
  return (
    <svg viewBox="0 0 16 16" width={size} height={size} fill="currentColor" aria-hidden="true">
      <path d="m8 14.25.345.666a.75.75 0 0 1-.69 0l-.008-.004-.018-.01a7.152 7.152 0 0 1-.31-.17 22.055 22.055 0 0 1-3.434-2.414C2.045 10.731 0 8.35 0 5.5 0 2.836 2.086 1 4.25 1 5.797 1 7.153 1.802 8 3.02 8.847 1.802 10.203 1 11.75 1 13.914 1 16 2.836 16 5.5c0 2.85-2.045 5.231-3.885 6.818a22.066 22.066 0 0 1-3.744 2.584l-.018.01-.006.003h-.002ZM4.25 2.5c-1.336 0-2.75 1.164-2.75 3 0 2.15 1.58 4.144 3.365 5.682A20.58 20.58 0 0 0 8 13.393a20.58 20.58 0 0 0 3.135-2.211C12.92 9.644 14.5 7.65 14.5 5.5c0-1.836-1.414-3-2.75-3-1.373 0-2.609.986-3.029 2.456a.749.749 0 0 1-1.442 0C6.859 3.486 5.623 2.5 4.25 2.5Z" />
    </svg>
  );
}
const sponsorsBase = (login: string) => `/ui-data/sponsors/${enc(login)}`;

export interface SponsorsTierRow {
  id: number;
  node_id: string;
  name: string;
  description: string;
  monthly_price_in_cents: number;
  monthly_price_in_dollars: number;
  is_one_time: boolean;
  is_custom_amount: boolean;
  is_draft: boolean;
  is_published: boolean;
  is_retired: boolean;
}

export interface SponsorsGoalRow {
  kind: string;
  target_value: number;
  percent_complete: number;
  description: string;
  title: string;
}

export interface SponsorsFeaturedItemRow {
  id: number;
  featureable_type: string;
  description: string;
  position: number;
  featureable?: { login?: string; name?: string; full_name?: string; description?: string };
}

export interface SponsorsListingRow {
  id: number;
  node_id: string;
  slug: string;
  name: string;
  sponsorable_login: string;
  sponsorable_type: string;
  short_description: string;
  full_description: string;
  is_public: boolean;
  patreon_enabled: boolean;
  fiscal_host: string;
  goal: SponsorsGoalRow | null;
  tiers: SponsorsTierRow[];
  featured_items: SponsorsFeaturedItemRow[];
  contact_email?: string;
  billing_country_or_region?: string;
  next_payout_date?: string;
  monthly_estimated_income_in_cents?: number;
  estimated_next_payout_in_cents?: number;
}

export interface SponsorshipRow {
  id: number;
  node_id: string;
  sponsor_login: string;
  sponsorable_login: string;
  privacy_level: string;
  is_one_time_payment: boolean;
  is_active: boolean;
  amount_in_cents: number;
  tier: SponsorsTierRow | null;
  pending_cancellation: boolean;
  pending_tier_id: number;
  next_billing_date: string | null;
  pending_effective_date: string | null;
}

interface SponsorsProfileResponse {
  listing: SponsorsListingRow | null;
  viewer_can_admin: boolean;
  viewer_sponsorship: SponsorshipRow | null;
  /** Public sponsors, plus any private one the viewer is a party to. */
  sponsors: SponsorshipRow[];
}

interface SponsorsDashboardResponse {
  listing: SponsorsListingRow;
  sponsorships: SponsorshipRow[];
  activities: { id: number; action: string; sponsor_login: string; timestamp: string }[];
  invoices: {
    id: number;
    sponsor_login: string;
    amount_in_cents: number;
    period_start: string;
    period_end: string;
    one_time: boolean;
    prorated: boolean;
    status: string;
    payout_id: number;
  }[];
  payouts: { id: number; amount_in_cents: number; status: string; scheduled_date: string; created_at: string }[];
  lifetime_values: { sponsor_login: string; amount_in_cents: number }[];
  monthly_estimated_income_in_cents: number;
  estimated_next_payout_in_cents: number;
}

const fetchSponsorsListings = (signal?: AbortSignal) =>
  ghFetch<SponsorsListingRow[]>("/ui-data/sponsors/listings", signal);
const fetchSponsorsProfile = (login: string, signal?: AbortSignal) =>
  ghFetch<SponsorsProfileResponse>(sponsorsBase(login), signal);
const fetchSponsorsDashboard = (login: string, signal?: AbortSignal) =>
  ghFetch<SponsorsDashboardResponse>(`${sponsorsBase(login)}/dashboard`, signal);
const fetchViewerSponsoring = (signal?: AbortSignal) =>
  ghFetch<SponsorshipRow[]>("/ui-data/sponsors/sponsoring", signal);

// Format integer cents as dollars without floating-point division.
export function formatCents(cents: number): string {
  const sign = cents < 0 ? "-" : "";
  const absolute = Math.abs(cents);
  return `${sign}$${Math.trunc(absolute / 100)}.${String(absolute % 100).padStart(2, "0")}`;
}

export function SponsorsPage() {
  const { login } = useParams<{ login?: string }>();
  return login ? <SponsorsProfile login={login} /> : <SponsorsDirectory />;
}

function SponsorsDirectory() {
  const listings = useQuery({ queryKey: ["sponsors", "listings"], queryFn: ({ signal }) => fetchSponsorsListings(signal) });
  const sponsoring = useQuery({ queryKey: ["sponsors", "sponsoring"], queryFn: ({ signal }) => fetchViewerSponsoring(signal) });

  if (listings.isLoading) return <Spinner label="loading GitHub Sponsors" />;
  if (listings.isError) return <InlineError title="Sponsors unavailable" detail={String(listings.error)} />;

  const rows = listings.data ?? [];
  const active = (sponsoring.data ?? []).filter((s) => s.is_active);
  const monthly = active.filter((s) => !s.is_one_time_payment).reduce((total, s) => total + s.amount_in_cents, 0);

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 style={{ fontSize: "1.5rem", fontWeight: 600, margin: 0 }}>
          <HeartIcon size={18} /> GitHub Sponsors
        </h1>
        <p style={{ color: "var(--color-fg-muted)", marginTop: ".25rem" }}>
          Invest in the developers and projects you depend on. Recurring and one-time sponsorships,
          tiers, goals and payouts are all real here; no payment processor is contacted.
        </p>
      </div>

      <div className="grid gap-3 sm:grid-cols-3">
        <StatCard title="Sponsorable accounts" value={rows.length} emphasized />
        <StatCard title="You are sponsoring" value={active.length} />
        <StatCard title="Your monthly total" value={formatCents(monthly)} />
      </div>

      <section>
        <SectionLabel>Sponsorable accounts</SectionLabel>
        {rows.length === 0 ? (
          <Blankslate title="No Sponsors profiles yet">
            Any user or organization can open a GitHub Sponsors profile from their account page.
          </Blankslate>
        ) : (
          <div className="flex flex-col gap-2">
            {rows.map((listing) => (
              <Box key={listing.id}>
                <div className="flex flex-wrap items-baseline justify-between gap-2" style={{ padding: "0.75rem 1rem" }}>
                  <div className="min-w-0">
                    <Link to={`/ui/sponsors/${enc(listing.sponsorable_login)}`} style={{ fontWeight: 600 }}>
                      {listing.name || listing.sponsorable_login}
                    </Link>
                    <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                      {listing.short_description || `Sponsor ${listing.sponsorable_login}`}
                    </div>
                  </div>
                  <div className="flex items-center gap-2">
                    {!listing.is_public && <StateLabel state="closed">Unpublished</StateLabel>}
                    <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
                      {listing.tiers.length} tier{listing.tiers.length === 1 ? "" : "s"}
                    </span>
                  </div>
                </div>
              </Box>
            ))}
          </div>
        )}
      </section>

      <section>
        <SectionLabel>Sponsorships you fund</SectionLabel>
        {active.length === 0 ? (
          <Blankslate title="You are not sponsoring anyone yet">
            Open a sponsorable account&apos;s profile and choose a tier.
          </Blankslate>
        ) : (
          <Box>
            <div style={{ overflowX: "auto" }}>
            <table className="w-full" style={{ fontSize: "0.9rem", minWidth: "30rem" }}>
              <caption className="sr-only">Sponsorships you currently fund</caption>
              <thead>
                <tr>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Maintainer</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Tier</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Amount</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Status</th>
                </tr>
              </thead>
              <tbody>
                {active.map((sponsorship) => (
                  <tr key={sponsorship.id}>
                    <td style={{ padding: "0.5rem 1rem" }}>
                      <Link to={`/ui/sponsors/${enc(sponsorship.sponsorable_login)}`}>{sponsorship.sponsorable_login}</Link>
                    </td>
                    <td style={{ padding: "0.5rem 1rem" }}>{sponsorship.tier?.name ?? "—"}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{formatCents(sponsorship.amount_in_cents)}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>
                      {sponsorship.pending_cancellation
                        ? "Ends at the end of the period"
                        : sponsorship.is_one_time_payment
                          ? "One-time"
                          : "Recurring"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            </div>
          </Box>
        )}
      </section>
    </div>
  );
}

function SponsorsProfile({ login }: { login: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const { data, isLoading, isError, error: loadError } = useQuery({
    queryKey: ["sponsors", "profile", login],
    queryFn: ({ signal }) => fetchSponsorsProfile(login, signal),
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["sponsors", "profile", login] });
    qc.invalidateQueries({ queryKey: ["sponsors", "sponsoring"] });
  };
  const sponsorMut = useMutation({
    mutationFn: (tier: SponsorsTierRow) =>
      ghPostJSON<SponsorshipRow>(`${sponsorsBase(login)}/sponsorship`, {
        tier_id: tier.id,
        is_recurring: !tier.is_one_time,
        privacy_level: "PUBLIC",
        receive_emails: true,
      }),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const changeMut = useMutation({
    mutationFn: (tier: SponsorsTierRow) =>
      ghSend("PATCH", `${sponsorsBase(login)}/sponsorship`, { tier_id: tier.id }),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const cancelMut = useMutation({
    mutationFn: () => ghSend("DELETE", `${sponsorsBase(login)}/sponsorship`),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const createListingMut = useMutation({
    mutationFn: () =>
      ghSend("PUT", sponsorsBase(login), {
        name: login,
        short_description: `Sponsor ${login}`,
        full_description: "",
        is_public: true,
      }),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });

  if (isLoading) return <Spinner label="loading the Sponsors profile" />;
  if (isError) return <InlineError title="Sponsors profile unavailable" detail={String(loadError)} />;

  const listing = data?.listing ?? null;
  const viewerSponsorship = data?.viewer_sponsorship ?? null;

  if (!listing) {
    return (
      <div className="flex flex-col gap-4">
        <h1 style={{ fontSize: "1.5rem", fontWeight: 600, margin: 0 }}>{login}</h1>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <Blankslate title="No GitHub Sponsors profile">
          {data?.viewer_can_admin
            ? "Open a Sponsors profile to start receiving recurring and one-time sponsorships."
            : `${login} has not opened a GitHub Sponsors profile.`}
        </Blankslate>
        {data?.viewer_can_admin && (
          <div>
            <Button onClick={() => createListingMut.mutate()} disabled={createListingMut.isPending}>
              Open a Sponsors profile
            </Button>
          </div>
        )}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 style={{ fontSize: "1.5rem", fontWeight: 600, margin: 0 }}>
            <HeartIcon size={18} /> Sponsor {listing.sponsorable_login}
          </h1>
          <p style={{ color: "var(--color-fg-muted)", marginTop: ".25rem" }}>
            {listing.short_description || listing.full_description}
          </p>
        </div>
        {data?.viewer_can_admin && (
          <Link to={`/ui/sponsors/${enc(login)}/dashboard`}>Sponsors dashboard →</Link>
        )}
      </div>

      {error && <ErrorBanner>{error}</ErrorBanner>}

      {listing.goal && <SponsorsGoalCard goal={listing.goal} />}

      {viewerSponsorship && (
        <Box>
          <div className="flex flex-wrap items-center justify-between gap-2" style={{ padding: "0.75rem 1rem" }}>
            <div>
              You are sponsoring {listing.sponsorable_login} at{" "}
              <strong>{formatCents(viewerSponsorship.amount_in_cents)}</strong>
              {viewerSponsorship.is_one_time_payment ? " (one time)" : " a month"}.
              {viewerSponsorship.pending_cancellation && " This sponsorship ends at the end of the current period."}
            </div>
            {!viewerSponsorship.pending_cancellation && (
              <Button
                variant="danger"
                onClick={async () => {
                  if (await confirmAction("Cancel this sponsorship at the end of the current period?")) {
                    cancelMut.mutate();
                  }
                }}
                disabled={cancelMut.isPending}
              >
                Cancel sponsorship
              </Button>
            )}
          </div>
        </Box>
      )}

      <section>
        <SectionLabel>Tiers</SectionLabel>
        {listing.tiers.length === 0 ? (
          <Blankslate title="No tiers yet">This profile has no published tiers.</Blankslate>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2">
            {listing.tiers.map((tier) => (
              <Box key={tier.id}>
                <div className="flex flex-col gap-2" style={{ padding: "0.75rem 1rem" }}>
                  <div className="flex items-baseline justify-between gap-2">
                    <strong>{tier.name}</strong>
                    {tier.is_draft && <StateLabel state="closed">Draft</StateLabel>}
                    {tier.is_retired && <StateLabel state="closed">Retired</StateLabel>}
                  </div>
                  <div style={{ fontSize: "1.1rem" }}>
                    {formatCents(tier.monthly_price_in_cents)}
                    <span style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                      {tier.is_one_time ? " one time" : " a month"}
                    </span>
                  </div>
                  <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>{tier.description}</div>
                  {tier.is_published && (
                    <div>
                      {viewerSponsorship && !viewerSponsorship.is_one_time_payment && !tier.is_one_time ? (
                        <Button
                          onClick={() => changeMut.mutate(tier)}
                          disabled={changeMut.isPending || viewerSponsorship.tier?.id === tier.id}
                        >
                          {viewerSponsorship.tier?.id === tier.id ? "Current tier" : "Switch to this tier"}
                        </Button>
                      ) : (
                        <Button onClick={() => sponsorMut.mutate(tier)} disabled={sponsorMut.isPending}>
                          Select
                        </Button>
                      )}
                    </div>
                  )}
                </div>
              </Box>
            ))}
          </div>
        )}
      </section>

      <section>
        <SectionLabel>Sponsors</SectionLabel>
        {(data?.sponsors ?? []).length === 0 ? (
          <Blankslate title="No public sponsors yet">
            Private sponsorships are shown only to the sponsor and the maintainer.
          </Blankslate>
        ) : (
          <Box>
            <ul style={{ margin: 0, padding: "0.75rem 1.5rem" }}>
              {(data?.sponsors ?? []).map((sponsorship) => (
                <li key={sponsorship.id}>
                  {sponsorship.sponsor_login} · {formatCents(sponsorship.amount_in_cents)}
                  {sponsorship.is_one_time_payment ? " one time" : " a month"}
                  {sponsorship.privacy_level === "PRIVATE" ? " (private)" : ""}
                </li>
              ))}
            </ul>
          </Box>
        )}
      </section>

      {listing.featured_items.length > 0 && (
        <section>
          <SectionLabel>Featured</SectionLabel>
          <Box>
            <ul style={{ margin: 0, padding: "0.75rem 1.5rem" }}>
              {listing.featured_items.map((item) => (
                <li key={item.id}>
                  {item.featureable?.full_name ?? item.featureable?.login ?? item.featureable_type}
                  {item.description ? ` — ${item.description}` : ""}
                </li>
              ))}
            </ul>
          </Box>
        </section>
      )}
    </div>
  );
}

function SponsorsGoalCard({ goal }: { goal: SponsorsGoalRow }) {
  return (
    <Box>
      <div className="flex flex-col gap-2" style={{ padding: "0.75rem 1rem" }}>
        <div className="flex items-baseline justify-between gap-2">
          <strong>{goal.title}</strong>
          <span style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>{goal.percent_complete}% complete</span>
        </div>
        <div
          role="progressbar"
          aria-valuenow={goal.percent_complete}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={goal.title}
          style={{ background: "var(--color-neutral-muted)", borderRadius: "999px", height: "0.5rem" }}
        >
          <div
            style={{
              width: `${goal.percent_complete}%`,
              background: "var(--color-success-fg)",
              borderRadius: "999px",
              height: "100%",
            }}
          />
        </div>
        {goal.description && (
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>{goal.description}</div>
        )}
      </div>
    </Box>
  );
}

export function SponsorsDashboardPage() {
  const { login = "" } = useParams<{ login: string }>();
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [tierAmount, setTierAmount] = useState("");
  const [tierDescription, setTierDescription] = useState("");
  const [goalTarget, setGoalTarget] = useState("");
  const [newsletterSubject, setNewsletterSubject] = useState("");

  const { data, isLoading, isError, error: loadError } = useQuery({
    queryKey: ["sponsors", "dashboard", login],
    queryFn: ({ signal }) => fetchSponsorsDashboard(login, signal),
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["sponsors", "dashboard", login] });
    qc.invalidateQueries({ queryKey: ["sponsors", "profile", login] });
  };
  const addTierMut = useMutation({
    mutationFn: () =>
      ghPostJSON<SponsorsTierRow>(`${sponsorsBase(login)}/tiers`, {
        amount_in_cents: Math.trunc(Number(tierAmount) * 100),
        description: tierDescription,
        publish: true,
      }),
    onSuccess: () => {
      invalidate();
      setTierAmount("");
      setTierDescription("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const retireTierMut = useMutation({
    mutationFn: (tierID: number) => ghSend("PATCH", `${sponsorsBase(login)}/tiers/${tierID}`, { state: "retired" }),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const setGoalMut = useMutation({
    mutationFn: () =>
      ghSend("PUT", `${sponsorsBase(login)}/goal`, {
        kind: "MONTHLY_SPONSORSHIP_AMOUNT",
        target_value: Math.trunc(Number(goalTarget) * 100),
      }),
    onSuccess: () => {
      invalidate();
      setGoalTarget("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const payoutMut = useMutation({
    mutationFn: () => ghPostJSON<{ id: number }>(`${sponsorsBase(login)}/payouts`, {}),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const newsletterMut = useMutation({
    mutationFn: () =>
      ghPostJSON<{ id: number }>(`${sponsorsBase(login)}/newsletters`, {
        subject: newsletterSubject,
        body: "",
        publish: true,
      }),
    onSuccess: () => {
      invalidate();
      setNewsletterSubject("");
    },
    onError: (err: Error) => setError(err.message),
  });

  if (isLoading) return <Spinner label="loading the Sponsors dashboard" />;
  if (isError) return <InlineError title="Sponsors dashboard unavailable" detail={String(loadError)} />;
  if (!data) return null;

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 style={{ fontSize: "1.5rem", fontWeight: 600, margin: 0 }}>Sponsors dashboard</h1>
        <p style={{ color: "var(--color-fg-muted)", marginTop: ".25rem" }}>
          <Link to={`/ui/sponsors/${enc(login)}`}>{login}</Link> · next payout {data.listing.next_payout_date ?? "—"}
        </p>
      </div>

      {error && <ErrorBanner>{error}</ErrorBanner>}

      <div className="grid gap-3 sm:grid-cols-3">
        <StatCard title="Monthly estimated income" value={formatCents(data.monthly_estimated_income_in_cents)} emphasized />
        <StatCard title="Awaiting payout" value={formatCents(data.estimated_next_payout_in_cents)} />
        <StatCard title="Sponsors" value={data.sponsorships.filter((s) => s.is_active).length} />
      </div>

      <section>
        <SectionLabel>Tiers</SectionLabel>
        <Box>
          <div className="flex flex-wrap items-end gap-2" style={{ padding: "0.75rem 1rem" }}>
            <div>
              <FormLabel id="sponsors-tier-amount">Amount (USD per month)</FormLabel>
              <input
                id="sponsors-tier-amount"
                type="number"
                min="1"
                value={tierAmount}
                onChange={(e) => setTierAmount(e.target.value)}
              />
            </div>
            <div>
              <FormLabel id="sponsors-tier-description">Description</FormLabel>
              <input
                id="sponsors-tier-description"
                value={tierDescription}
                onChange={(e) => setTierDescription(e.target.value)}
              />
            </div>
            <Button onClick={() => addTierMut.mutate()} disabled={addTierMut.isPending || !tierAmount}>
              Add tier
            </Button>
          </div>
        </Box>
        <div className="mt-2 flex flex-col gap-2">
          {data.listing.tiers.map((tier) => (
            <Box key={tier.id}>
              <div className="flex flex-wrap items-center justify-between gap-2" style={{ padding: "0.5rem 1rem" }}>
                <span>
                  <strong>{tier.name}</strong> · {formatCents(tier.monthly_price_in_cents)}
                  {tier.is_one_time ? " one time" : " a month"}
                </span>
                <span className="flex items-center gap-2">
                  {tier.is_retired ? (
                    <StateLabel state="closed">Retired</StateLabel>
                  ) : (
                    <Button onClick={() => retireTierMut.mutate(tier.id)} disabled={retireTierMut.isPending}>
                      Retire
                    </Button>
                  )}
                </span>
              </div>
            </Box>
          ))}
        </div>
      </section>

      <section>
        <SectionLabel>Goal</SectionLabel>
        {data.listing.goal && <SponsorsGoalCard goal={data.listing.goal} />}
        <Box>
          <div className="mt-2 flex flex-wrap items-end gap-2" style={{ padding: "0.75rem 1rem" }}>
            <div>
              <FormLabel id="sponsors-goal-target">Monthly goal (USD)</FormLabel>
              <input
                id="sponsors-goal-target"
                type="number"
                min="1"
                value={goalTarget}
                onChange={(e) => setGoalTarget(e.target.value)}
              />
            </div>
            <Button onClick={() => setGoalMut.mutate()} disabled={setGoalMut.isPending || !goalTarget}>
              Set goal
            </Button>
          </div>
        </Box>
      </section>

      <section>
        <SectionLabel>Sponsors</SectionLabel>
        {data.sponsorships.length === 0 ? (
          <Blankslate title="No sponsors yet">Share your profile to start receiving sponsorships.</Blankslate>
        ) : (
          <Box>
            <div style={{ overflowX: "auto" }}>
            <table className="w-full" style={{ fontSize: "0.9rem", minWidth: "30rem" }}>
              <caption className="sr-only">Sponsorships funding this account</caption>
              <thead>
                <tr>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Sponsor</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Tier</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Amount</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Privacy</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>State</th>
                </tr>
              </thead>
              <tbody>
                {data.sponsorships.map((sponsorship) => (
                  <tr key={sponsorship.id}>
                    <td style={{ padding: "0.5rem 1rem" }}>{sponsorship.sponsor_login}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{sponsorship.tier?.name ?? "—"}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{formatCents(sponsorship.amount_in_cents)}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{sponsorship.privacy_level}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>
                      {!sponsorship.is_active
                        ? "Cancelled"
                        : sponsorship.pending_cancellation
                          ? "Ending"
                          : sponsorship.pending_tier_id
                            ? "Tier change pending"
                            : "Active"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            </div>
          </Box>
        )}
      </section>

      <section>
        <SectionLabel>Invoices</SectionLabel>
        {data.invoices.length === 0 ? (
          <Blankslate title="Nothing billed yet">Invoices appear as sponsorships bill each period.</Blankslate>
        ) : (
          <Box>
            <div style={{ overflowX: "auto" }}>
            <table className="w-full" style={{ fontSize: "0.9rem", minWidth: "30rem" }}>
              <caption className="sr-only">Sponsorship invoices</caption>
              <thead>
                <tr>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Sponsor</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Amount</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Period start</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Kind</th>
                  <th scope="col" style={{ textAlign: "left", padding: "0.5rem 1rem" }}>Paid out</th>
                </tr>
              </thead>
              <tbody>
                {data.invoices.map((invoice) => (
                  <tr key={invoice.id}>
                    <td style={{ padding: "0.5rem 1rem" }}>{invoice.sponsor_login}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{formatCents(invoice.amount_in_cents)}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>{invoice.period_start.slice(0, 10)}</td>
                    <td style={{ padding: "0.5rem 1rem" }}>
                      {invoice.one_time ? "One time" : invoice.prorated ? "Prorated" : "Recurring"}
                    </td>
                    <td style={{ padding: "0.5rem 1rem" }}>{invoice.payout_id ? `#${invoice.payout_id}` : "Pending"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            </div>
          </Box>
        )}
      </section>

      <section>
        <SectionLabel>Payouts</SectionLabel>
        <Box>
          <div className="flex flex-wrap items-center justify-between gap-2" style={{ padding: "0.75rem 1rem" }}>
            <span>
              {formatCents(data.estimated_next_payout_in_cents)} is awaiting payout.
            </span>
            <Button
              onClick={() => payoutMut.mutate()}
              disabled={payoutMut.isPending || data.estimated_next_payout_in_cents === 0}
            >
              Run payout
            </Button>
          </div>
        </Box>
        {data.payouts.length > 0 && (
          <div className="mt-2 flex flex-col gap-2">
            {data.payouts.map((payout) => (
              <Box key={payout.id}>
                <div style={{ padding: "0.5rem 1rem" }}>
                  #{payout.id} · {formatCents(payout.amount_in_cents)} · {payout.status} · scheduled {payout.scheduled_date}
                </div>
              </Box>
            ))}
          </div>
        )}
      </section>

      <section>
        <SectionLabel>Sponsor updates</SectionLabel>
        <Box>
          <div className="flex flex-wrap items-end gap-2" style={{ padding: "0.75rem 1rem" }}>
            <div>
              <FormLabel id="sponsors-newsletter-subject">Subject</FormLabel>
              <input
                id="sponsors-newsletter-subject"
                value={newsletterSubject}
                onChange={(e) => setNewsletterSubject(e.target.value)}
              />
            </div>
            <Button
              onClick={() => newsletterMut.mutate()}
              disabled={newsletterMut.isPending || !newsletterSubject.trim()}
            >
              Publish update
            </Button>
          </div>
        </Box>
      </section>

      <section>
        <SectionLabel>Activity</SectionLabel>
        {data.activities.length === 0 ? (
          <Blankslate title="No activity yet">Sponsorship events show up here as they happen.</Blankslate>
        ) : (
          <Box>
            <ul style={{ margin: 0, padding: "0.75rem 1.5rem" }}>
              {data.activities.map((activity) => (
                <li key={activity.id}>
                  {activity.action} · {activity.sponsor_login} · {activity.timestamp.slice(0, 10)}
                </li>
              ))}
            </ul>
          </Box>
        )}
      </section>
    </div>
  );
}
