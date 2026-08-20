import { Blankslate } from "./ui.js";

/**
 * GitHub-style 404 for admin-only URLs (repo settings, branch protection,
 * secrets). github.com answers these routes with "not found" for viewers
 * without admin access rather than revealing that the page exists, so the
 * guard pages render this once permissions have loaded.
 */
export function RepoNotFound() {
  return (
    <Blankslate title="This page does not exist">
      <p>The page you are looking for could not be found.</p>
    </Blankslate>
  );
}
