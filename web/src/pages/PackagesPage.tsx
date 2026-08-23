import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { DataTable, InlineError, Spinner } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import { RelativeTime } from "../components/RelativeTime.js";
import {
  deletePackage,
  deletePackageVersion,
  fetchCurrentUser,
  fetchPackageFiles,
  fetchPackages,
  fetchPackageVersions,
  restorePackageVersion,
  packageListPath,
  ghFetch,
  ghPostJSON,
  type PackageScope,
} from "../api.js";
import type {
  GithubPackage,
  GithubPackageVersion,
  GithubPackageType,
} from "../types.js";
import {
  Button,
  CodeBlock,
  DialogActions,
  ErrorBanner,
  Modal,
  PageTitle,
  Tabs,
} from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { PackageIcon, TrashIcon } from "../components/octicons.js";

const PACKAGE_TYPES: GithubPackageType[] = [
  "npm",
  "maven",
  "rubygems",
  "nuget",
  "docker",
  "container",
];

const pkgCol = createColumnHelper<GithubPackage>();
const verCol = createColumnHelper<GithubPackageVersion>();

export function PackagesPage() {
  const params = useParams<{ org?: string; owner?: string; repo?: string }>();
  const [tab, setTab] = useState<GithubPackageType>("container");

  const {
    data: currentUser,
    isLoading: userLoading,
    isError: userError,
    error: userErrorObj,
  } = useQuery({
    queryKey: ["current-user"],
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    enabled: !params.org && !(params.owner && params.repo),
  });

  const scope: PackageScope | null = params.org
    ? { kind: "org", org: params.org }
    : params.owner && params.repo
      ? { kind: "repo", owner: params.owner, repo: params.repo }
      : currentUser
        ? { kind: "user", username: currentUser.login }
        : null;

  if (userLoading) {
    return <Spinner label="loading user" />;
  }
  if (userError) {
    return (
      <InlineError title="Failed to load current user" detail={String(userErrorObj)} />
    );
  }
  if (!scope) {
    return <InlineError title="Unable to determine package scope" />;
  }

  return (
    <div>
      <PageTitle
        icon={<PackageIcon size={20} />}
        title="Packages"
        meta="Manage packages and versions."
      />

      <Tabs<GithubPackageType>
        items={PACKAGE_TYPES.map((t) => ({ key: t, label: t }))}
        active={tab}
        onChange={setTab}
      />

      <PackagesList scope={scope} packageType={tab} />
    </div>
  );
}

function PackagesList({
  scope,
  packageType,
}: {
  scope: PackageScope;
  packageType: GithubPackageType;
}) {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<GithubPackage | null>(null);

  const listKey = ["packages", scope, packageType];
  const {
    data,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: listKey,
    queryFn: () => fetchPackages(scope, packageType),
  });

  const deletePkgMut = useMutation({
    mutationFn: (pkg: GithubPackage) =>
      deletePackage(scope, pkg.package_type, pkg.name),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: listKey }),
  });

  const filtered = useMemo(
    () => (data ?? []).filter((p) => p.package_type === packageType),
    [data, packageType],
  );

  const columns = useMemo(
    () => [
      pkgCol.accessor("name", {
        header: "Name",
        cell: (info) => (
          <button
            type="button"
            onClick={() => setSelected(info.row.original)}
            className="font-medium"
            style={{
              background: "transparent",
              border: "none",
              padding: 0,
              color: "var(--color-accent)",
              cursor: "pointer",
            }}
          >
            {info.getValue()}
          </button>
        ),
      }),
      pkgCol.accessor("visibility", { header: "Visibility" }),
      pkgCol.accessor("version_count", {
        header: "Versions",
        cell: (info) => (
          <span className="tabular-nums">{info.getValue()}</span>
        ),
      }),
      pkgCol.accessor("updated_at", {
        header: "Updated",
        cell: (info) => <RelativeTime iso={info.getValue<string>()} />,
      }),
      pkgCol.display({
        id: "actions",
        header: "Actions",
        cell: (info) => {
          const pkg = info.row.original;
          return (
            <div className="flex flex-wrap items-center gap-2">
              <Button
                size="sm"
                variant="secondary"
                onClick={() => setSelected(pkg)}
              >
                Versions
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={async () => {
                  if (await confirmAction(`Delete package ${pkg.name}?`)) {
                    deletePkgMut.mutate(pkg);
                  }
                }}
                disabled={deletePkgMut.isPending}
              >
                <TrashIcon size={14} /> Delete
              </Button>
            </div>
          );
        },
      }),
    ],
    [deletePkgMut, scope],
  );

  if (isError) {
    return (
      <InlineError title="Failed to load packages" detail={String(error)} />
    );
  }
  if (isLoading || !data) {
    return <Spinner label="loading packages" />;
  }

  return (
    <>
      <MutationError of={deletePkgMut} />
      <div className="mb-3 flex items-center justify-between">
        <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
          {filtered.length} package{filtered.length === 1 ? "" : "s"}
        </div>
      </div>
      <DataTable
        data={filtered}
        columns={columns}
        emptyMessage="No packages yet."
      />
      {scope.kind !== "repo" && <DeletedPackages scope={scope} packageType={packageType} />}
      {selected && (
        <PackageDetailDialog
          scope={scope}
          pkg={selected}
          onClose={() => setSelected(null)}
        />
      )}
    </>
  );
}

// Deleted packages (github.com's "?state=deleted") with a per-package Restore.
// Only user/org scopes have a restore endpoint (there is no repo-scoped one).
function packageRestorePath(scope: PackageScope, pkgType: string, name: string): string | null {
  const seg = `packages/${encodeURIComponent(pkgType)}/${encodeURIComponent(name)}/restore`;
  switch (scope.kind) {
    case "user":
      return `/api/v3/users/${encodeURIComponent(scope.username)}/${seg}`;
    case "org":
      return `/api/v3/orgs/${encodeURIComponent(scope.org)}/${seg}`;
    case "repo":
      return null;
  }
}

function DeletedPackages({ scope, packageType }: { scope: PackageScope; packageType: GithubPackageType }) {
  const queryClient = useQueryClient();
  const key = ["packages-deleted", scope, packageType];
  const { data } = useQuery({
    queryKey: key,
    queryFn: () => ghFetch<GithubPackage[]>(`${packageListPath(scope, packageType)}&state=deleted`),
  });
  const restoreMut = useMutation({
    mutationFn: (pkg: GithubPackage) => {
      const path = packageRestorePath(scope, pkg.package_type, pkg.name);
      return path ? ghPostJSON<void>(path, {}) : Promise.reject(new Error("no restore endpoint"));
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: key });
      queryClient.invalidateQueries({ queryKey: ["packages", scope, packageType] });
    },
  });
  const deleted = (data ?? []).filter((p) => p.package_type === packageType);
  if (deleted.length === 0) return null;
  return (
    <section className="mt-6">
      <h3 style={{ fontSize: "0.9rem", fontWeight: 600, color: "var(--color-fg-muted)", marginBottom: "0.5rem" }}>
        Deleted packages
      </h3>
      <MutationError of={restoreMut} />
      <ul style={{ listStyle: "none", margin: 0, padding: 0, border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)" }}>
        {deleted.map((pkg, i) => (
          <li
            key={pkg.name}
            className="flex items-center justify-between gap-3"
            style={{ padding: "0.6rem 0.9rem", borderBottom: i === deleted.length - 1 ? "none" : "1px solid var(--color-border)" }}
          >
            <span style={{ fontSize: "0.88rem" }}>
              {pkg.name} <span style={{ color: "var(--color-fg-muted)" }}>({pkg.package_type})</span>
            </span>
            <Button
              size="sm"
              variant="secondary"
              aria-label={`Restore package ${pkg.name}`}
              disabled={restoreMut.isPending}
              onClick={async () => {
                if (await confirmAction(`Restore package ${pkg.name}?`, { title: "Restore package", confirmLabel: "Restore" })) {
                  restoreMut.mutate(pkg);
                }
              }}
            >
              Restore
            </Button>
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * GitHub-style per-ecosystem install command. Covers every package type the
 * server's store accepts (npm/maven/rubygems/nuget/docker/container); the
 * container/docker pull path uses this host as the registry.
 */
function installCommand(pkg: GithubPackage, latestVersion: string | undefined): string {
  const host = window.location.host;
  const ownerLogin = pkg.owner?.login ?? "OWNER";
  const v = latestVersion;
  switch (pkg.package_type) {
    case "docker":
    case "container":
      return `docker pull ${host}/${ownerLogin}/${pkg.name}${v ? `:${v}` : ""}`;
    case "npm":
      return `npm install ${pkg.name}${v ? `@${v}` : ""}`;
    case "maven": {
      // Maven package names are "<groupId>.<artifactId>".
      const dot = pkg.name.lastIndexOf(".");
      const groupId = dot > 0 ? pkg.name.slice(0, dot) : ownerLogin;
      const artifactId = dot > 0 ? pkg.name.slice(dot + 1) : pkg.name;
      return [
        "<dependency>",
        `  <groupId>${groupId}</groupId>`,
        `  <artifactId>${artifactId}</artifactId>`,
        `  <version>${v ?? "VERSION"}</version>`,
        "</dependency>",
      ].join("\n");
    }
    case "nuget":
      return `dotnet add package ${pkg.name}${v ? ` --version ${v}` : ""}`;
    case "rubygems":
      return `gem install ${pkg.name}${v ? ` --version "${v}"` : ""}`;
  }
}

function PackageDetailDialog({
  scope,
  pkg,
  onClose,
}: {
  scope: PackageScope;
  pkg: GithubPackage;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);

  const versionsKey = ["package-versions", scope, pkg.package_type, pkg.name];
  const {
    data: versions,
    isLoading,
    isError,
  } = useQuery({
    queryKey: versionsKey,
    queryFn: () => fetchPackageVersions(scope, pkg.package_type, pkg.name),
  });

  const deleteMut = useMutation({
    mutationFn: (v: GithubPackageVersion) =>
      deletePackageVersion(scope, pkg.package_type, pkg.name, v.id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: versionsKey });
      queryClient.invalidateQueries({ queryKey: ["packages", scope, pkg.package_type] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const restoreMut = useMutation({
    mutationFn: (v: GithubPackageVersion) =>
      restorePackageVersion(scope, pkg.package_type, pkg.name, v.id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: versionsKey });
      queryClient.invalidateQueries({ queryKey: ["packages", scope, pkg.package_type] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const columns = useMemo(
    () => [
      verCol.accessor("name", { header: "Version" }),
      verCol.accessor("description", {
        header: "Description",
        cell: (info) => info.getValue() || "—",
      }),
      // The package-version payload carries no download counters (GitHub's
      // REST schema has none either), so timestamps are the row's metadata.
      verCol.accessor("created_at", {
        header: "Published",
        cell: (info) => <RelativeTime iso={info.getValue<string>()} />,
      }),
      verCol.display({
        id: "files",
        header: "Files",
        cell: (info) => <VersionFiles scope={scope} pkg={pkg} version={info.row.original} />,
      }),
      verCol.display({
        id: "actions",
        header: "Actions",
        cell: (info) => {
          const v = info.row.original;
          const deleted = !!v.deleted_at;
          return (
            <div className="flex flex-wrap items-center gap-2">
              {deleted ? (
                // GitHub has no repository-scoped package-version restore endpoint —
                // restore is only available for user- and organization-scoped packages.
                scope.kind !== "repo" && (
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => restoreMut.mutate(v)}
                    disabled={restoreMut.isPending}
                  >
                    Restore
                  </Button>
                )
              ) : (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={async () => {
                    if (await confirmAction(`Delete version ${v.name}?`)) {
                      deleteMut.mutate(v);
                    }
                  }}
                  disabled={deleteMut.isPending}
                >
                  <TrashIcon size={14} /> Delete
                </Button>
              )}
            </div>
          );
        },
      }),
    ],
    [deleteMut, restoreMut, scope, pkg],
  );

  const latestVersion = versions?.find((v) => !v.deleted_at)?.name;

  return (
    <Modal title={`${pkg.name} versions`} onClose={onClose}>
      <section className="mb-4">
        <h3 style={{ fontSize: "0.86rem", fontWeight: 600, marginBottom: "0.35rem" }}>
          Installation
        </h3>
        <CodeBlock>{installCommand(pkg, latestVersion)}</CodeBlock>
        {pkg.repository && (
          <div className="mt-2" style={{ fontSize: "0.82rem" }}>
            Source repository:{" "}
            <Link
              to={`/ui/${pkg.repository.full_name}`}
              style={{ color: "var(--color-accent)", textDecoration: "none" }}
            >
              {pkg.repository.full_name}
            </Link>
          </div>
        )}
      </section>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {isError ? (
        <InlineError title="Failed to load versions" />
      ) : isLoading || !versions ? (
        <Spinner label="loading versions" />
      ) : (
        <DataTable
          data={versions}
          columns={columns}
          emptyMessage="No versions."
        />
      )}
      <DialogActions>
        <Button onClick={onClose} variant="ghost">
          Close
        </Button>
      </DialogActions>
    </Modal>
  );
}

function VersionFiles({
  scope,
  pkg,
  version,
}: {
  scope: PackageScope;
  pkg: GithubPackage;
  version: GithubPackageVersion;
}) {
  const { data, isLoading } = useQuery({
    queryKey: ["package-files", scope, pkg.package_type, pkg.name, version.id],
    queryFn: () => fetchPackageFiles(scope, pkg.package_type, pkg.name, version.id),
  });

  if (isLoading || !data) return <span style={{ fontSize: "0.78rem" }}>…</span>;
  return (
    <span style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
      {data.length} file{data.length === 1 ? "" : "s"}
    </span>
  );
}
