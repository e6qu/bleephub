import { isNotFound } from "../api.js";

// True on an HTTP 404. Covers ApiError (via isNotFound) plus any object
// carrying `status: 404`, since a few fetchers throw other shapes.
export function isNotFoundError(err: unknown): boolean {
  if (isNotFound(err)) return true;
  return typeof err === "object" && err !== null && (err as { status?: unknown }).status === 404;
}
