import { expect, test } from "./fixtures.js";
import { createAppAuth } from "@octokit/auth-app";
import { Octokit } from "octokit";
import { randomUUID } from "crypto";

const BASE_URL = "http://localhost:15555";
const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";

async function adminRequest(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set("Authorization", `Bearer ${ADMIN_TOKEN}`);
  return fetch(`${BASE_URL}${path}`, { ...init, headers });
}

test("Octokit app auth searches the private repositories selected by its installation", async () => {
  const suffix = randomUUID().slice(0, 18);
  const repositoryName = `octokit-installation-${suffix}`;
  const marker = `octokit-private-search-${suffix}`;

  const createRepository = await adminRequest("/api/v3/user/repos", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      name: repositoryName,
      description: marker,
      private: true,
      auto_init: true,
    }),
  });
  expect(createRepository.status).toBe(201);
  const repository = await createRepository.json() as { id: number; full_name: string };

  const manifest = new URLSearchParams({
    manifest: JSON.stringify({
      name: `Octokit installation ${suffix}`,
      url: "https://example.test/octokit",
      redirect_url: "https://example.test/octokit/complete",
      default_permissions: {},
    }),
  });
  const registerApp = await adminRequest("/settings/apps/new", {
    method: "POST",
    redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: manifest,
  });
  expect(registerApp.status).toBe(302);
  const redirect = registerApp.headers.get("location");
  expect(redirect).toBeTruthy();
  const conversionCode = new URL(redirect!, BASE_URL).searchParams.get("code");
  expect(conversionCode).toBeTruthy();

  const convertApp = await fetch(
    `${BASE_URL}/api/v3/app-manifests/${encodeURIComponent(conversionCode!)}/conversions`,
    { method: "POST" },
  );
  expect(convertApp.status).toBe(201);
  const app = await convertApp.json() as {
    id: number;
    slug: string;
    pem: string;
    permissions: Record<string, string>;
  };
  expect(app.permissions.metadata).toBe("read");

  const installationForm = new URLSearchParams({
    target_login: "admin",
    repository_selection: "selected",
    repository_ids: String(repository.id),
  });
  const installApp = await adminRequest(`/apps/${encodeURIComponent(app.slug)}/installations/new`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: installationForm,
  });
  expect(installApp.status).toBe(201);
  const installation = await installApp.json() as { id: number };

  // This is Octokit's documented GitHub App shape. createAppAuth signs a JWT,
  // calls POST /app/installations/{id}/access_tokens, caches the returned ghs_
  // token, then sends that installation token on search.repos.
  const octokit = new Octokit({
    authStrategy: createAppAuth,
    auth: {
      appId: app.id,
      privateKey: app.pem,
      installationId: installation.id,
    },
    baseUrl: `${BASE_URL}/api/v3`,
  });

  const result = await octokit.rest.search.repos({
    q: `repo:${repository.full_name} ${marker}`,
  });
  expect(result.status).toBe(200);
  expect(result.data.total_count).toBe(1);
  expect(result.data.items.map((item) => item.full_name)).toEqual([repository.full_name]);

  const installationRepositories =
    await octokit.rest.apps.listReposAccessibleToInstallation();
  expect(installationRepositories.data.total_count).toBe(1);
  expect(installationRepositories.data.repositories.map((item) => item.full_name))
    .toEqual([repository.full_name]);
});

test("Octokit installation auth provisions an organization team and repository grant", async () => {
  const suffix = randomUUID().slice(0, 18);
  const organization = `octokit-team-${suffix}`;
  const repositoryName = `managed-${suffix}`;

  const createOrganization = await adminRequest("/api/v3/admin/organizations", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ login: organization, admin: "admin" }),
  });
  expect(createOrganization.status).toBe(201);

  const manifest = new URLSearchParams({
    manifest: JSON.stringify({
      name: `Octokit team provisioner ${suffix}`,
      url: "https://example.test/octokit-team",
      redirect_url: "https://example.test/octokit-team/complete",
      default_permissions: {
        administration: "write",
        members: "write",
      },
    }),
  });
  const registerApp = await adminRequest("/settings/apps/new", {
    method: "POST",
    redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: manifest,
  });
  expect(registerApp.status).toBe(302);
  const redirect = registerApp.headers.get("location");
  expect(redirect).toBeTruthy();
  const conversionCode = new URL(redirect!, BASE_URL).searchParams.get("code");
  expect(conversionCode).toBeTruthy();

  const convertApp = await fetch(
    `${BASE_URL}/api/v3/app-manifests/${encodeURIComponent(conversionCode!)}/conversions`,
    { method: "POST" },
  );
  expect(convertApp.status).toBe(201);
  const app = await convertApp.json() as { id: number; slug: string; pem: string };

  const installationForm = new URLSearchParams({
    target_login: organization,
    repository_selection: "all",
  });
  const installApp = await adminRequest(`/apps/${encodeURIComponent(app.slug)}/installations/new`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: installationForm,
  });
  expect(installApp.status).toBe(201);
  const installation = await installApp.json() as { id: number };

  const octokit = new Octokit({
    authStrategy: createAppAuth,
    auth: {
      appId: app.id,
      privateKey: app.pem,
      installationId: installation.id,
    },
    baseUrl: `${BASE_URL}/api/v3`,
  });

  const createdRepository = await octokit.rest.repos.createInOrg({
    org: organization,
    name: repositoryName,
    auto_init: true,
  });
  expect(createdRepository.status).toBe(201);

  const createdTeam = await octokit.rest.teams.create({
    org: organization,
    name: "Platform",
  });
  expect(createdTeam.status).toBe(201);
  expect(createdTeam.data.slug).toBe("platform");

  const grant = await octokit.rest.teams.addOrUpdateRepoPermissionsInOrg({
    org: organization,
    team_slug: createdTeam.data.slug,
    owner: organization,
    repo: repositoryName,
    permission: "push",
  });
  expect(grant.status).toBe(204);

  const managedRepositories = await octokit.rest.teams.listReposInOrg({
    org: organization,
    team_slug: createdTeam.data.slug,
  });
  expect(managedRepositories.data.map((repository) => repository.full_name))
    .toEqual([`${organization}/${repositoryName}`]);

  const removeGrant = await octokit.rest.teams.removeRepoInOrg({
    org: organization,
    team_slug: createdTeam.data.slug,
    owner: organization,
    repo: repositoryName,
  });
  expect(removeGrant.status).toBe(204);

  const removeTeam = await octokit.rest.teams.deleteInOrg({
    org: organization,
    team_slug: createdTeam.data.slug,
  });
  expect(removeTeam.status).toBe(204);
});
