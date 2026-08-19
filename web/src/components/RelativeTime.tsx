import { formatAbsolute, formatRelative } from "../utils/relativeTime.js";

/**
 * GitHub-style timestamp: relative wording up to ~a month ("2 hours ago"),
 * absolute after ("on Aug 19, 2026"), with the full timestamp on hover and a
 * machine-readable dateTime. Renders nothing for a missing/unparsable value
 * rather than a bogus 1970 date.
 */
export function RelativeTime({ iso }: { iso: string | null | undefined }) {
  if (!iso) return null;
  const text = formatRelative(iso);
  if (!text) return null;
  return (
    <time dateTime={iso} title={formatAbsolute(iso)}>
      {text}
    </time>
  );
}
