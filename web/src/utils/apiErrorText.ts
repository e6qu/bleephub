// Fetch-layer messages embed the raw response ("PUT 405: {json}"); surface the
// API's own `message` field when present instead of that debugging text.
export function humanApiError(err: unknown, fallback: string): string {
  if (!(err instanceof Error) || !err.message) return fallback;
  const embedded = err.message.match(/\{.*\}\s*$/s);
  if (embedded) {
    try {
      const parsed = JSON.parse(embedded[0]) as { message?: unknown };
      if (typeof parsed.message === "string" && parsed.message) return parsed.message;
    } catch {
      // fall through
    }
  }
  return err.message;
}
