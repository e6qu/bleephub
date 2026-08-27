import { Link, useLocation } from "react-router";
import { loginPath } from "../session.js";

/**
 * Signed-out stand-in for a write surface: a "Sign in" link that returns the
 * visitor to the current page. Rendered in place of composers for anonymous
 * visitors.
 */
export function SignInPrompt({ action = "comment" }: { action?: string }) {
  const location = useLocation();
  return (
    <div
      style={{
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-md)",
        background: "var(--color-bg-subtle)",
        padding: "1rem",
        textAlign: "center",
        fontSize: "0.9rem",
        color: "var(--color-fg-muted)",
      }}
    >
      <Link
        to={loginPath(location)}
        style={{ color: "var(--color-accent)", fontWeight: 600, textDecoration: "none" }}
      >
        Sign in
      </Link>{" "}
      to {action}
    </div>
  );
}
