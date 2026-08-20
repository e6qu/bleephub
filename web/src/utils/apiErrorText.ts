/**
 * The fetch layer's error messages embed the raw response ("PUT 405: {json}"),
 * which is debugging text, not UI copy. Surface the API's own `message` field
 * ("Pull Request is not mergeable") when one is present.
 */
export function humanApiError(err: unknown, fallback: string): string {
  if (!(err instanceof Error) || !err.message) return fallback;
  const embedded = err.message.match(/\{.*\}\s*$/s);
  if (embedded) {
    try {
      const parsed = JSON.parse(embedded[0]) as { message?: unknown };
      if (typeof parsed.message === "string" && parsed.message) return parsed.message;
    } catch {
      // fall through to the raw message
    }
  }
  return err.message;
}
