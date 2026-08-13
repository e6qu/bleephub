import { useMemo } from "react";
import type { GithubUserEvent } from "../types.js";

/**
 * A GitHub-style contribution calendar built from a user's activity events.
 *
 * The simulator's events feed only surfaces a handful of derived event types
 * (Create/Delete/Push/Issues/IssueComment/PullRequest), so this is an
 * approximation of GitHub's commit-based calendar — but it is real data, one
 * cell per day for the trailing 53 weeks, shaded by that day's event count.
 * Intensity uses `color-mix` over `--color-accent` so it themes in both modes.
 */

const WEEKS = 53;
const DAY_MS = 24 * 60 * 60 * 1000;
const WEEKDAY_LABELS = ["", "Mon", "", "Wed", "", "Fri", ""];
const MONTH_LABELS = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];

function dayKey(d: Date): string {
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

function levelColor(level: number): string {
  if (level === 0) return "var(--color-bg-subtle)";
  const pct = [0, 25, 45, 70, 100][level];
  return `color-mix(in srgb, var(--color-accent) ${pct}%, var(--color-bg-subtle))`;
}

export function ContributionGraph({ events }: { events: GithubUserEvent[] }) {
  const { weeks, total, monthTicks } = useMemo(() => {
    const counts = new Map<string, number>();
    for (const e of events) {
      const t = Date.parse(e.created_at);
      if (Number.isNaN(t)) continue;
      const d = new Date(t);
      counts.set(dayKey(d), (counts.get(dayKey(d)) ?? 0) + 1);
    }

    // Anchor the last column on the current week (grid ends on Saturday).
    const today = new Date();
    const end = new Date(today.getFullYear(), today.getMonth(), today.getDate());
    end.setDate(end.getDate() + (6 - end.getDay())); // forward to Saturday
    const start = new Date(end.getTime() - (WEEKS * 7 - 1) * DAY_MS);

    const cols: { date: Date; count: number; level: number }[][] = [];
    const ticks: { col: number; label: string }[] = [];
    let running = 0;
    let lastMonth = -1;
    for (let w = 0; w < WEEKS; w++) {
      const col: { date: Date; count: number; level: number }[] = [];
      for (let day = 0; day < 7; day++) {
        const date = new Date(start.getTime() + (w * 7 + day) * DAY_MS);
        const inFuture = date.getTime() > today.getTime();
        const count = inFuture ? 0 : counts.get(dayKey(date)) ?? 0;
        running += count;
        const level = count === 0 ? 0 : count >= 8 ? 4 : count >= 5 ? 3 : count >= 2 ? 2 : 1;
        col.push({ date, count, level });
        if (day === 0 && date.getMonth() !== lastMonth) {
          lastMonth = date.getMonth();
          ticks.push({ col: w, label: MONTH_LABELS[date.getMonth()] ?? "" });
        }
      }
      cols.push(col);
    }
    return { weeks: cols, total: running, monthTicks: ticks };
  }, [events]);

  const cell = 11;
  const gap = 3;

  return (
    <div>
      <div
        style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.5rem" }}
      >
        {total} contribution{total === 1 ? "" : "s"} in the last year
      </div>
      <div style={{ overflowX: "auto" }}>
        <table
          role="grid"
          aria-readonly="true"
          aria-label="User activity over the last year, one cell per day"
          style={{ borderCollapse: "separate", borderSpacing: `${gap}px` }}
        >
          <thead>
            <tr style={{ height: 14 }}>
              <td style={{ width: 28 }} />
              {weeks.map((_, w) => {
                const tick = monthTicks.find((t) => t.col === w);
                return (
                  <td key={w} style={{ fontSize: "0.68rem", color: "var(--color-fg-muted)", textAlign: "left" }}>
                    {tick ? tick.label : ""}
                  </td>
                );
              })}
            </tr>
          </thead>
          <tbody>
            {[0, 1, 2, 3, 4, 5, 6].map((day) => (
              <tr key={day}>
                <td
                  style={{
                    fontSize: "0.66rem",
                    color: "var(--color-fg-muted)",
                    paddingRight: 4,
                    textAlign: "right",
                    height: cell,
                  }}
                >
                  {WEEKDAY_LABELS[day]}
                </td>
                {weeks.map((col, w) => {
                  const c = col[day]!;
                  return (
                    <td
                      key={w}
                      title={`${c.count} contribution${c.count === 1 ? "" : "s"} on ${c.date.toLocaleDateString()}`}
                      style={{
                        width: cell,
                        height: cell,
                        borderRadius: 2,
                        background: levelColor(c.level),
                        border: "1px solid color-mix(in srgb, var(--color-border) 60%, transparent)",
                      }}
                    />
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div
        className="mt-2 flex items-center gap-1"
        style={{ fontSize: "0.68rem", color: "var(--color-fg-muted)", justifyContent: "flex-end" }}
      >
        <span style={{ marginRight: 2 }}>Less</span>
        {[0, 1, 2, 3, 4].map((l) => (
          <span
            key={l}
            style={{
              width: cell,
              height: cell,
              borderRadius: 2,
              background: levelColor(l),
              border: "1px solid color-mix(in srgb, var(--color-border) 60%, transparent)",
            }}
          />
        ))}
        <span style={{ marginLeft: 2 }}>More</span>
      </div>
    </div>
  );
}
