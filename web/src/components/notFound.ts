import { isNotFound } from "../api.js";

/**
 * True when `err` is an HTTP 404 on a page's primary resource — the signal to
 * render a GitHub-style "does not exist" page instead of the raw error banner.
 * api.ts fetchers throw ApiError (covered by isNotFound); a few fetchers throw
 * other shapes, so any object carrying `status: 404` counts too. Everything
 * else (500s, network failures, parse errors) stays on the generic banner.
 */
export function isNotFoundError(err: unknown): boolean {
  if (isNotFound(err)) return true;
  return typeof err === "object" && err !== null && (err as { status?: unknown }).status === 404;
}
