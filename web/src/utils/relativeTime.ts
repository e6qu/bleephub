const MINUTE = 60;
const HOUR = 3600;
const DAY = 86400;
const WEEK = 7 * DAY;
const MONTH = 30 * DAY;

function plural(value: number, unit: string): string {
  return `${value} ${unit}${value === 1 ? "" : "s"} ago`;
}

/** Absolute wording past one month; year omitted inside the current year. */
export function formatOnDate(date: Date, now: Date): string {
  const sameYear = date.getFullYear() === now.getFullYear();
  const text = date.toLocaleDateString("en-US", {
    month: "short",
    day: "numeric",
    ...(sameYear ? {} : { year: "numeric" }),
  });
  return `on ${text}`;
}

export function formatRelative(iso: string, now: Date = new Date()): string {
  if (!iso) return "";
  const date = new Date(iso);
  const then = date.getTime();
  if (!Number.isFinite(then)) return "";
  const secs = Math.round((now.getTime() - then) / 1000);
  if (secs < 45) return "just now";
  if (secs < 45 * MINUTE) return plural(Math.max(1, Math.round(secs / MINUTE)), "minute");
  if (secs < 24 * HOUR) return plural(Math.max(1, Math.round(secs / HOUR)), "hour");
  if (secs < WEEK) return plural(Math.max(1, Math.round(secs / DAY)), "day");
  if (secs < MONTH) return plural(Math.max(1, Math.round(secs / WEEK)), "week");
  return formatOnDate(date, now);
}

/** Full timestamp for the hover title. */
export function formatAbsolute(iso: string): string {
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return "";
  return date.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
    timeZoneName: "short",
  });
}
