import { type ReactNode, useEffect, useState } from "react";
import { AppHeader } from "./AppHeader.js";
import { fetchHealth } from "../api.js";

// RepoHeader/OrgHeader live in ./PageHeader.js so their heavy imports stay out of the entry chunk.

/** Global header (AppHeader) above the routed page content. */
export function BleephubShell({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-screen flex-col" style={{ background: "var(--color-bg)", color: "var(--color-fg)" }}>
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only"
        style={{
          position: "absolute",
          top: 8,
          left: 8,
          padding: "0.4rem 0.75rem",
          background: "var(--color-accent)",
          color: "var(--color-accent-fg)",
          fontSize: "0.8rem",
          zIndex: 100,
          borderRadius: "var(--radius-md)",
        }}
      >
        Skip to main content
      </a>
      <AppHeader />
      <main id="main-content" tabIndex={-1} className="mx-auto w-full max-w-[1280px] flex-1 px-4 py-6">
        {children}
      </main>
      <BleephubBuildFooter />
    </div>
  );
}

const buildVersion = import.meta.env.VITE_BLEEPHUB_VERSION || "development";
const publishedAt = import.meta.env.VITE_BLEEPHUB_PUBLISHED_AT || "not yet published";

/** Release identity shown on every surface, including sign-in. */
export function BleephubBuildFooter() {
  const [identity, setIdentity] = useState({ version: buildVersion, publishedAt });
  useEffect(() => {
    let current = true;
    fetchHealth()
      .then((health) => {
        if (!current) return;
        setIdentity({
          version: health.version || buildVersion,
          publishedAt: health.published_at || publishedAt,
        });
      })
      .catch(() => {
        // Footer is informational; never surface a failed health probe.
      });
    return () => {
      current = false;
    };
  }, []);
  const publishedLabel = formatPublishedAt(identity.publishedAt);
  return (
    <footer
      data-testid="bleephub-build-footer"
      className="mx-auto flex w-full max-w-[1280px] flex-wrap items-center justify-between gap-x-4 gap-y-1 px-4 py-5"
      style={{ borderTop: "1px solid var(--color-border)", color: "var(--color-fg-muted)", fontSize: "0.75rem" }}
    >
      <span>Bleephub {identity.version}</span>
      {identity.publishedAt === "not yet published" ? (
        <span>Unreleased build</span>
      ) : (
        <time dateTime={identity.publishedAt}>Published {publishedLabel}</time>
      )}
    </footer>
  );
}

function formatPublishedAt(value: string) {
  if (value === "not yet published") return value;
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short", timeZone: "UTC" }) + " UTC";
}

