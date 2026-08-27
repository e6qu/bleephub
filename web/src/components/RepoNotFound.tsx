import { Blankslate } from "./ui.js";

// 404 for admin-only URLs; github.com hides existence from non-admin viewers.
export function RepoNotFound() {
  return (
    <Blankslate title="This page does not exist">
      <p>The page you are looking for could not be found.</p>
    </Blankslate>
  );
}
