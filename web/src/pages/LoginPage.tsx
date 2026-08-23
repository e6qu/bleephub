import { useEffect, useState } from "react";
import { ApiError, createTokenBrowserSession } from "../api.js";
import { Mark } from "../components/octicons.js";
import { Button, ErrorBanner } from "../components/ui.js";
import { BleephubBuildFooter } from "../components/Shell.js";

function describeFailure(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function returnDestination(): string {
  const candidate = new URLSearchParams(window.location.search).get("return_to");
  if (!candidate) return "/ui/";
  try {
    const parsed = new URL(candidate, window.location.origin);
    if (parsed.origin !== window.location.origin) return "/ui/";
    if (parsed.pathname === "/control" || parsed.pathname === "/ui" || parsed.pathname.startsWith("/ui/")) {
      return parsed.pathname + parsed.search + parsed.hash;
    }
  } catch {
    // Invalid input falls through to the dashboard.
  }
  return "/ui/";
}

export function LoginPage() {
  const [token, setTokenValue] = useState("");
  const [error, setError] = useState("");
  const [verifying, setVerifying] = useState(false);
  const [providers, setProviders] = useState<{ github?: boolean; shauth?: boolean } | null>(null);
  const [providersError, setProvidersError] = useState<string | null>(null);
  const [login, setLogin] = useState("");
  const [password, setPassword] = useState("");
  const [localSigningIn, setLocalSigningIn] = useState(false);

  useEffect(() => {
    void (async () => {
      try {
        const response = await fetch("/auth/providers");
        if (!response.ok) {
          throw new Error(`${response.status} ${response.statusText}`);
        }
        setProviders((await response.json()) as { github?: boolean; shauth?: boolean });
      } catch (err) {
        // An unreachable provider list is not the same as an instance with no
        // providers configured. Token sign-in still works, so fall through to
        // it — but say why the other options are missing instead of just
        // rendering none of them.
        setProvidersError(describeFailure(err));
        setProviders({});
      }
    })();
  }, []);

  async function handleSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setError("");
    setVerifying(true);
    try {
      await createTokenBrowserSession(token);
      setTokenValue("");
      window.location.href = returnDestination();
      return;
    } catch (err) {
      setError(
        err instanceof ApiError && (err.status === 401 || err.status === 403)
          ? "Token rejected. Bleephub accepts only an active user access token."
          : `Could not reach Bleephub to exchange the token: ${describeFailure(err)}`,
      );
    }
    setVerifying(false);
  }

  async function handleLocalSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setError("");
    setLocalSigningIn(true);
    try {
      const response = await fetch("/auth/local", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ login, password }),
      });
      if (response.ok) {
        window.location.href = returnDestination();
        return;
      }
      setError("Local credentials were not accepted.");
    } catch (err) {
      setError(`Could not reach Bleephub to sign in: ${describeFailure(err)}`);
    }
    setLocalSigningIn(false);
  }

  const returnTo = new URLSearchParams(window.location.search).get("return_to");
  const githubHref = `/auth/github?return_to=${encodeURIComponent(
    returnTo?.startsWith("/") && !returnTo.startsWith("//") ? returnTo : "/ui/",
  )}`;
  const shauthHref = `/auth/shauth?return_to=${encodeURIComponent(
    returnTo?.startsWith("/") && !returnTo.startsWith("//") ? returnTo : "/ui/",
  )}`;

  useEffect(() => {
    if (providers?.shauth) {
      window.location.href = shauthHref;
    }
  }, [providers, shauthHref]);

  if (providers === null || providers.shauth) {
    return (
      <div
        className="flex min-h-screen flex-col px-4"
        // G10: flat neutral canvas in both themes, like github.com's sign-in
        // page — no blue/pink radial wash.
        style={{ background: "var(--color-bg-subtle)" }}
      >
        <div className="flex w-full flex-1 items-center justify-center">
          <main
            className="w-full max-w-sm"
            style={{
              border: "1px solid var(--color-border)",
              borderRadius: "1rem",
              background: "var(--color-surface)",
              padding: "1.5rem",
              textAlign: "center",
              boxShadow: "var(--shadow-floating)",
            }}
            aria-labelledby="bleephub-sign-in-title"
            aria-live="polite"
          >
            <Mark size={42} />
            <h1 id="bleephub-sign-in-title" style={{ marginTop: ".7rem", fontSize: "1.4rem", fontWeight: 650, color: "var(--color-fg)" }}>
              {providers?.shauth ? "Sign in to Bleephub" : "Preparing sign-in…"}
            </h1>
            {providers?.shauth && (
              <>
                <p style={{ margin: ".65rem 0 1rem", color: "var(--color-fg-muted)", fontSize: ".88rem" }}>
                  Use your shared e6qu identity to continue.
                </p>
                <a
                  href={shauthHref}
                  className="inline-flex min-h-11 w-full items-center justify-center"
                  style={{
                    border: "1px solid var(--color-accent)",
                    borderRadius: "var(--radius-md)",
                    background: "var(--color-accent)",
                    color: "var(--color-accent-fg)",
                    fontWeight: 700,
                    textDecoration: "none",
                  }}
                >
                  Sign in with Shauth
                </a>
              </>
            )}
          </main>
        </div>
        <BleephubBuildFooter />
      </div>
    );
  }

  return (
    <div
      className="flex min-h-screen flex-col items-center justify-center px-4"
      style={{ background: "var(--color-bg-subtle)" }}
    >
      <div className="flex w-full flex-1 flex-col items-center justify-center">
        <div className="mb-5 flex flex-col items-center gap-2">
          <Mark size={42} />
          <h1 style={{ fontSize: "1.4rem", fontWeight: 600, color: "var(--color-fg)" }}>
            Sign in to Bleephub
          </h1>
        </div>
        <div
          className="w-full max-w-sm"
          style={{
            border: "1px solid var(--color-border)",
            borderRadius: "var(--radius-md)",
            background: "var(--color-surface)",
            padding: "1.25rem",
          }}
        >
        {providers.github && (
          <a
            href={githubHref}
            className="mb-3 flex w-full items-center justify-center"
            style={{
              border: "1px solid var(--color-border)",
              borderRadius: "var(--radius-md)",
              minHeight: "2.25rem",
              color: "var(--color-fg)",
              fontWeight: 600,
              textDecoration: "none",
            }}
          >
            Continue with GitHub
          </a>
        )}
        <form onSubmit={handleLocalSubmit} className="mb-4">
          <label htmlFor="login" className="mb-1 block" style={{ fontSize: "0.85rem", fontWeight: 600, color: "var(--color-fg)" }}>
            Local account
          </label>
          <input id="login" value={login} onChange={(e) => setLogin(e.target.value)} placeholder="login" disabled={localSigningIn} className="mb-2 w-full" />
          <input aria-label="Local password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="password" disabled={localSigningIn} className="mb-2 w-full" />
          <Button type="submit" variant="ghost" disabled={localSigningIn || !login || !password} style={{ width: "100%" }}>
            {localSigningIn ? "Signing in…" : "Continue with local account"}
          </Button>
        </form>
        <div className="mb-4" style={{ borderTop: "1px solid var(--color-border)" }} />
        <form onSubmit={handleSubmit}>
          <label
            htmlFor="token"
            className="mb-1 block"
            style={{ fontSize: "0.85rem", fontWeight: 600, color: "var(--color-fg)" }}
          >
            Access token
          </label>
          <input
            id="token"
            type="password"
            value={token}
            onChange={(e) => setTokenValue(e.target.value)}
            placeholder="GitHub-compatible token"
            autoFocus
            disabled={verifying}
            className="mb-1 w-full"
            style={{
              fontFamily: "var(--font-mono)",
              fontSize: "0.85rem",
            }}
          />
          <p className="mb-3" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
            Use the admin token, a personal access token, or an OAuth token accepted by this Bleephub instance.
          </p>
          {providersError && (
            <ErrorBanner>
              Could not load the available sign-in methods, so only token sign-in is offered:{" "}
              {providersError}
            </ErrorBanner>
          )}
          {error && (
            <p className="mb-3" style={{ fontSize: "0.82rem", color: "var(--color-status-error)" }}>
              {error}
            </p>
          )}
          <Button
            type="submit"
            variant="primary"
            disabled={verifying || !token}
            style={{ width: "100%", opacity: verifying || !token ? 0.6 : 1 }}
          >
            {verifying ? "Verifying…" : "Sign in"}
          </Button>
        </form>
        </div>
      </div>
      <BleephubBuildFooter />
    </div>
  );
}
