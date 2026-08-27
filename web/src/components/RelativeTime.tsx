import { formatAbsolute, formatRelative } from "../utils/relativeTime.js";

/** Renders nothing for a missing/unparsable value (avoids a 1970 date). */
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
