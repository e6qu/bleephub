import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import sodium from "libsodium-wrappers";
import { AccountPage } from "../pages/AccountPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

// useTheme (Appearance tab) reads prefers-color-scheme; jsdom has no matchMedia.
Object.defineProperty(window, "matchMedia", {
  configurable: true,
  value: vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
});

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function installFetchRoutes(overrides: Record<string, () => Response> = {}) {
  mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    const key = `${method} ${url}`;
    if (overrides[key]) return Promise.resolve(overrides[key]());
    if (key === "GET /api/v3/user/keys")
      return Promise.resolve(
        jsonResponse([
          {
            id: 3,
            key: "ssh-ed25519 AAAA...",
            title: "laptop",
            verified: true,
            created_at: "2026-05-01T00:00:00Z",
            read_only: false,
          },
        ]),
      );
    if (key === "GET /api/v3/user/gpg_keys") return Promise.resolve(jsonResponse([]));
    if (key === "GET /api/v3/authorizations") return Promise.resolve(jsonResponse([]));
    if (key === "GET /settings/personal-access-tokens")
      return Promise.resolve(
        jsonResponse({ tokens: [], resource_owners: [{ login: "admin", type: "User" }], repositories: {}, pending_requests: [] }),
      );
    if (key === "GET /api/v3/user/ssh_signing_keys") return Promise.resolve(jsonResponse([]));
    if (key === "GET /api/v3/user/emails")
      return Promise.resolve(
        jsonResponse([
          { email: "admin@example.com", primary: true, verified: true, visibility: "private" },
          { email: "alt@example.com", primary: false, verified: false, visibility: null },
        ]),
      );
    if (key === "GET /api/v3/user/blocks")
      return Promise.resolve(jsonResponse([{ login: "spammer" }]));
    if (key === "GET /api/v3/user/codespaces/secrets")
      return Promise.resolve(
        jsonResponse({
          total_count: 1,
          secrets: [
            { name: "NPM_TOKEN", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-02T00:00:00Z", visibility: "all" },
          ],
        }),
      );
    // Public profile tab (the default) fetches the viewer then their profile.
    if (key === "GET /api/v3/user")
      return Promise.resolve(jsonResponse({ id: 1, login: "admin", type: "User", site_admin: true }));
    if (url.includes("/api/v3/users/admin") && method === "GET")
      return Promise.resolve(jsonResponse({ login: "admin", name: "Admin", bio: "", company: "", location: "", blog: "", twitter_username: "", created_at: "2026-01-01T00:00:00Z", followers: 0, following: 0, public_repos: 0 }));
    if (key === "PATCH /api/v3/user")
      return Promise.resolve(jsonResponse({ login: "admin", name: "Admin" }));
    if (url === "/ui-data/user/authentication")
      return Promise.resolve(
        jsonResponse({
          authentication: { kind: "local", password_set: true },
          two_factor: {
            enabled: false,
            pending_enrollment: false,
            recovery_codes_total: 0,
            recovery_codes_remaining: 0,
          },
        }),
      );
    if (url === "/ui-data/user/sessions")
      return Promise.resolve(
        jsonResponse({
          sessions: [
            {
              handle: "handle-1",
              created_at: "2026-05-01T00:00:00Z",
              expires_at: "2026-05-02T00:00:00Z",
              user_agent: "Mozilla/5.0 (Test)",
              ip: "192.0.2.7",
              provider: "",
              current: true,
            },
          ],
        }),
      );
    if (url === "/ui-data/user/two-factor/enrollment" && method === "POST")
      return Promise.resolve(
        jsonResponse(
          {
            secret: "JBSWY3DPEHPK3PXP",
            otpauth_uri: "otpauth://totp/bleephub:admin?secret=JBSWY3DPEHPK3PXP",
            issuer: "bleephub",
            account: "admin",
            digits: 6,
            period: 30,
            qr: { size: 3, modules: ["101", "010", "101"] },
          },
          201,
        ),
      );
    if (url === "/ui-data/user/two-factor/enrollment/confirm")
      return Promise.resolve(
        jsonResponse({
          authentication: { kind: "local", password_set: true },
          two_factor: {
            enabled: true,
            pending_enrollment: false,
            enrolled_at: "2026-05-01T00:00:00Z",
            recovery_codes_total: 2,
            recovery_codes_remaining: 2,
          },
          recovery_codes: ["abcde-fghjk", "mnpqr-stuvw"],
        }),
      );
    if (url === "/ui-data/user/notification-settings")
      return Promise.resolve(
        jsonResponse({
          participating: { email: true, web: true },
          watching: { email: true, web: true },
          automatically_watch_repositories: true,
          automatically_watch_teams: false,
          include_own_updates: false,
          actions_failed_workflows_only: true,
          dependabot_weekly_digest: true,
          events: {
            issue: { email: true, web: true },
            pull_request: { email: true, web: true },
            release: { email: true, web: true },
            discussion: { email: true, web: true },
            commit: { email: true, web: true },
            actions: { email: true, web: true },
            dependabot: { email: true, web: true },
          },
        }),
      );
    if (key.startsWith("PUT /ui-data/user/")) return Promise.resolve(jsonResponse({}));
    return Promise.resolve(jsonResponse({ message: `unexpected ${key}` }, 500));
  });
}

function renderPage(initialEntry = "/ui/account") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <AccountPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("AccountPage", () => {
  it("enrolls in two-factor by scanning a code and proving it, never by a toggle", async () => {
    installFetchRoutes();
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Password and authentication" }));
    await waitFor(() => expect(screen.getByText(/Two-factor authentication is/)).toBeInTheDocument());

    // Starting enrollment must not claim protection — it returns a QR + setup key, then asks for proof.
    fireEvent.click(screen.getByRole("button", { name: /Enable two-factor/ }));
    await waitFor(() => expect(screen.getByText("Setup key")).toBeInTheDocument());
    expect(screen.getByRole("img", { name: /QR code enrolling admin/ })).toBeInTheDocument();
    expect(screen.getByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();
    expect(
      mockFetch.mock.calls.some(
        ([u, i]) =>
          String(u) === "/ui-data/user/two-factor/enrollment" &&
          (i as RequestInit | undefined)?.method === "POST",
      ),
    ).toBe(true);

    // The code enables it; recovery codes show exactly once afterward.
    fireEvent.change(screen.getByLabelText("Verification code"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: /Verify and enable/ }));
    await waitFor(() => expect(screen.getByText("Save your recovery codes")).toBeInTheDocument());
    expect(screen.getByText("abcde-fghjk")).toBeInTheDocument();
    const confirmCall = mockFetch.mock.calls.find(
      ([u]) => String(u) === "/ui-data/user/two-factor/enrollment/confirm",
    );
    expect(JSON.parse(String((confirmCall?.[1] as RequestInit).body))).toEqual({ code: "123456" });
  });

  it("changes the password through PUT /ui-data/user/password", async () => {
    installFetchRoutes();
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Password and authentication" }));
    await waitFor(() => expect(screen.getByLabelText("Old password")).toBeInTheDocument());
    fireEvent.change(screen.getByLabelText("Old password"), { target: { value: "old-password-1" } });
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "new-password-12" } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "new-password-12" } });
    fireEvent.click(screen.getByRole("button", { name: "Change password" }));
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(([u]) => String(u) === "/ui-data/user/password");
      expect(call).toBeDefined();
      expect(JSON.parse(String((call?.[1] as RequestInit).body))).toEqual({
        current_password: "old-password-1",
        new_password: "new-password-12",
      });
    });
  });

  it("lists active sessions and revokes one by handle", async () => {
    installFetchRoutes();
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Password and authentication" }));
    await waitFor(() => expect(screen.getByText(/Mozilla\/5.0 \(Test\)/)).toBeInTheDocument());
    expect(screen.getByText(/192.0.2.7/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    fireEvent.click(await screen.findByRole("button", { name: "End session" }));
    await waitFor(() =>
      expect(
        mockFetch.mock.calls.some(
          ([u, i]) =>
            String(u) === "/ui-data/user/sessions/handle-1" &&
            (i as RequestInit | undefined)?.method === "DELETE",
        ),
      ).toBe(true),
    );
  });

  it("tells a federated account its credentials live with the identity provider", async () => {
    installFetchRoutes({
      "GET /ui-data/user/authentication": () =>
        jsonResponse({
          authentication: { kind: "external", providers: ["https://idp.example"], password_set: false },
          two_factor: {
            enabled: false,
            pending_enrollment: false,
            recovery_codes_total: 0,
            recovery_codes_remaining: 0,
          },
        }),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Password and authentication" }));
    await waitFor(() =>
      expect(screen.getByText(/signs in through an identity provider/)).toBeInTheDocument(),
    );
    expect(screen.getByText("https://idp.example")).toBeInTheDocument();
    // No control that could not possibly work is offered.
    expect(screen.queryByRole("button", { name: /Enable two-factor/ })).toBeNull();
    expect(screen.queryByLabelText("New password")).toBeNull();
  });

  it("saves a per-event-type notification channel via PUT /ui-data/user/notification-settings", async () => {
    installFetchRoutes();
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Notifications" }));
    await waitFor(() => expect(screen.getByText("Notification types")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("checkbox", { name: "Web notifications for Pull requests" }));
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(
        ([u, i]) =>
          String(u) === "/ui-data/user/notification-settings" &&
          (i as RequestInit | undefined)?.method === "PUT",
      );
      expect(call).toBeDefined();
      const body = JSON.parse(String((call?.[1] as RequestInit).body)) as {
        events: Record<string, { email: boolean; web: boolean }>;
      };
      expect(body.events.pull_request).toEqual({ email: true, web: false });
      expect(body.events.issue).toEqual({ email: true, web: true });
    });
  });

  it("lists SSH keys from GET /user/keys", async () => {
    installFetchRoutes();
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "SSH keys" }));
    await waitFor(() => {
      expect(screen.getByText("laptop")).toBeInTheDocument();
    });
    expect(screen.getByText(/verified · added/)).toBeInTheDocument();
  });

  it("renders a left settings sub-nav and marks the active item", async () => {
    installFetchRoutes();
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "SSH keys" }));
    await waitFor(() => screen.getByText("laptop"));

    const nav = screen.getByRole("navigation", { name: "Settings" });
    expect(nav).toBeInTheDocument();
    expect(screen.getByText("Access")).toBeInTheDocument();
    expect(screen.getByText("Moderation")).toBeInTheDocument();

    const sshItem = screen.getByRole("button", { name: "SSH keys" });
    expect(sshItem).toHaveAttribute("aria-current", "page");

    fireEvent.click(screen.getByRole("button", { name: "Emails" }));
    await waitFor(() => screen.getByText("admin@example.com"));
    expect(screen.getByRole("button", { name: "Emails" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("button", { name: "SSH keys" })).not.toHaveAttribute("aria-current");
  });

  it("adds an SSH key via POST /user/keys", async () => {
    installFetchRoutes({
      "POST /api/v3/user/keys": () =>
        jsonResponse(
          {
            id: 4,
            key: "ssh-rsa BBBB...",
            title: "desktop",
            verified: true,
            created_at: "2026-06-01T00:00:00Z",
            read_only: false,
          },
          201,
        ),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "SSH keys" }));
    await waitFor(() => screen.getByText("laptop"));

    fireEvent.change(document.getElementById("user-ssh-keys-title")!, {
      target: { value: "desktop" },
    });
    fireEvent.change(document.getElementById("user-ssh-keys-key")!, {
      target: { value: "ssh-rsa BBBB..." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add SSH key" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find((c) => c[1]?.method === "POST");
      expect(post).toBeDefined();
      expect(String(post![0])).toBe("/api/v3/user/keys");
      expect(post![1].body).toContain("ssh-rsa BBBB...");
    });
  });

  it("creates a repository-scoped fine-grained token and shows its credential once", async () => {
    const dashboard = {
      tokens: [],
      resource_owners: [{ login: "admin", type: "User" }],
      repositories: { admin: [{ id: 7, name: "release", private: true }] },
      pending_requests: [],
    };
    installFetchRoutes({
      "GET /settings/personal-access-tokens": () => jsonResponse(dashboard),
      "POST /settings/personal-access-tokens": () => jsonResponse({
        id: 11, name: "Release automation", resource_owner: "admin",
        repository_selection: "all", repository_ids: [], permissions: { repository: { contents: "read" } },
        created_at: "2026-07-12T00:00:00Z", expires_at: null, status: "active",
        token: "github_pat_once_only",
      }, 201),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "SSH keys" }));
    await waitFor(() => screen.getByText("laptop"));
    fireEvent.click(screen.getByRole("button", { name: "Personal access tokens" }));
    expect(await screen.findByText("Fine-grained personal access tokens")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Token name"), { target: { value: "Release automation" } });
    fireEvent.click(screen.getByRole("button", { name: "Generate token" }));
    expect(await screen.findByText("github_pat_once_only")).toBeInTheDocument();
    const post = mockFetch.mock.calls.find((call) => call[1]?.method === "POST" && String(call[0]) === "/settings/personal-access-tokens");
    expect(post).toBeDefined();
    expect(post![1].body).toContain('"resource_owner":"admin"');
    expect(post![1].body).toContain('"contents":"read"');
  });

  it("defaults to Public profile and saves edits via PATCH /user", async () => {
    installFetchRoutes();
    renderPage();
    // Public profile is the default tab (matches github.com).
    expect(screen.getByRole("button", { name: "Public profile" })).toHaveAttribute("aria-current", "page");
    // The profile form loads asynchronously (viewer → profile fetch).
    const saveBtn = await screen.findByRole("button", { name: /update profile/i });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Ada Lovelace" } });
    fireEvent.click(saveBtn);
    await waitFor(() => {
      const patch = mockFetch.mock.calls.find((c) => c[1]?.method === "PATCH" && String(c[0]) === "/api/v3/user");
      expect(patch).toBeDefined();
      expect(String(patch![1].body)).toContain("Ada Lovelace");
    });
  });

  it("adds a social account via POST /user/social_accounts", async () => {
    installFetchRoutes({
      "GET /api/v3/user/social_accounts": () => jsonResponse([]),
      "POST /api/v3/user/social_accounts": () =>
        jsonResponse([{ provider: "generic", url: "https://example.com/me" }], 201),
    });
    renderPage();
    // Public profile is the default tab; the social accounts editor lives here.
    const input = await screen.findByLabelText("Add a social link");
    fireEvent.change(input, { target: { value: "https://example.com/me" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[1]?.method === "POST" && String(c[0]) === "/api/v3/user/social_accounts",
      );
      expect(post).toBeDefined();
      expect(String(post![1].body)).toContain("https://example.com/me");
      expect(String(post![1].body)).toContain("account_urls");
    });
  });

  it("has an Appearance tab that switches the theme", async () => {
    installFetchRoutes();
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Appearance" }));
    const dark = await screen.findByRole("radio", { name: /dark/i });
    fireEvent.click(dark);
    expect(document.documentElement.classList.contains("dark")).toBe(true);
  });

  it("deletes an SSH key via DELETE /user/keys/{id}", async () => {
    installFetchRoutes({
      "DELETE /api/v3/user/keys/3": () => new Response(null, { status: 204 }),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "SSH keys" }));
    await waitFor(() => screen.getByText("laptop"));

    fireEvent.click(screen.getByRole("button", { name: "delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find((c) => c[1]?.method === "DELETE");
      expect(del).toBeDefined();
      expect(String(del![0])).toBe("/api/v3/user/keys/3");
    });
  });

  it("shows emails with visibility and toggles the primary visibility", async () => {
    installFetchRoutes({
      "PATCH /api/v3/user/email/visibility": () =>
        jsonResponse([
          { email: "admin@example.com", primary: true, verified: true, visibility: "public" },
        ]),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Emails" }));
    await waitFor(() => {
      expect(screen.getByText("admin@example.com")).toBeInTheDocument();
    });
    expect(screen.getByText(/primary · verified · visibility: private/)).toBeInTheDocument();
    expect(screen.getByText(/unverified · visibility unset/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Make public" }));
    await waitFor(() => {
      const patch = mockFetch.mock.calls.find((c) => c[1]?.method === "PATCH");
      expect(patch).toBeDefined();
      expect(String(patch![0])).toBe("/api/v3/user/email/visibility");
      expect(patch![1].body).toContain('"visibility":"public"');
    });
  });

  it("adds and removes email addresses", async () => {
    installFetchRoutes({
      "POST /api/v3/user/emails": () =>
        jsonResponse(
          [{ email: "new@example.com", primary: false, verified: false, visibility: null }],
          201,
        ),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Emails" }));
    await waitFor(() => screen.getByText("admin@example.com"));

    fireEvent.change(screen.getByLabelText("New email address"), {
      target: { value: "new@example.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find((c) => c[1]?.method === "POST");
      expect(post).toBeDefined();
      expect(String(post![0])).toBe("/api/v3/user/emails");
      expect(post![1].body).toContain("new@example.com");
    });
  });

  it("lists and unblocks blocked users", async () => {
    installFetchRoutes({
      "DELETE /api/v3/user/blocks/spammer": () => new Response(null, { status: 204 }),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Blocked users" }));
    await waitFor(() => {
      expect(screen.getByText("spammer")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: "unblock" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find((c) => c[1]?.method === "DELETE");
      expect(del).toBeDefined();
      expect(String(del![0])).toBe("/api/v3/user/blocks/spammer");
    });
  });

  it("lists Codespaces secrets and seals a new one via PUT /user/codespaces/secrets/{name}", async () => {
    await sodium.ready;
    const keypair = sodium.crypto_box_keypair();
    installFetchRoutes({
      "GET /api/v3/user/codespaces/secrets/public-key": () =>
        jsonResponse({
          key_id: "568250167242549743",
          key: sodium.to_base64(keypair.publicKey, sodium.base64_variants.ORIGINAL),
        }),
      "PUT /api/v3/user/codespaces/secrets/MY_SECRET": () => new Response(null, { status: 204 }),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Codespaces" }));
    await waitFor(() => screen.getByText("NPM_TOKEN"));

    fireEvent.change(document.getElementById("codespaces-secret-name")!, {
      target: { value: "MY_SECRET" },
    });
    fireEvent.change(document.getElementById("codespaces-secret-value")!, {
      target: { value: "super-plain-text" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) =>
          String(c[0]) === "/api/v3/user/codespaces/secrets/MY_SECRET" &&
          (c[1] as RequestInit | undefined)?.method === "PUT",
      );
      expect(put).toBeDefined();
      const rawBody = String((put![1] as RequestInit).body);
      // The plaintext must never appear on the wire.
      expect(rawBody).not.toContain("super-plain-text");
      const body = JSON.parse(rawBody);
      expect(body.key_id).toBe("568250167242549743");
      const opened = sodium.crypto_box_seal_open(
        sodium.from_base64(body.encrypted_value, sodium.base64_variants.ORIGINAL),
        keypair.publicKey,
        keypair.privateKey,
      );
      expect(sodium.to_string(opened)).toBe("super-plain-text");
    });
  });

  it("deletes a Codespaces secret via DELETE /user/codespaces/secrets/{name}", async () => {
    installFetchRoutes({
      "DELETE /api/v3/user/codespaces/secrets/NPM_TOKEN": () => new Response(null, { status: 204 }),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Codespaces" }));
    await waitFor(() => screen.getByText("NPM_TOKEN"));

    fireEvent.click(screen.getByRole("button", { name: "delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) =>
          String(c[0]) === "/api/v3/user/codespaces/secrets/NPM_TOKEN" &&
          (c[1] as RequestInit | undefined)?.method === "DELETE",
      );
      expect(del).toBeDefined();
    });
  });

  it("blocks a user via PUT /user/blocks/{username}", async () => {
    installFetchRoutes({
      "PUT /api/v3/user/blocks/troll": () => new Response(null, { status: 204 }),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Blocked users" }));
    await waitFor(() => screen.getByLabelText("Username to block"));

    fireEvent.change(screen.getByLabelText("Username to block"), { target: { value: "troll" } });
    fireEvent.click(screen.getByRole("button", { name: "Block" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find((c) => c[1]?.method === "PUT");
      expect(put).toBeDefined();
      expect(String(put![0])).toBe("/api/v3/user/blocks/troll");
    });
  });

  it("renders billing usage summary and fires GET .../settings/billing/usage/summary", async () => {
    installFetchRoutes({
      "GET /api/v3/users/admin/settings/billing/usage/summary": () =>
        jsonResponse({
          timePeriod: { year: 2026, month: 8 },
          user: "admin",
          usageItems: [
            {
              product: "actions",
              sku: "Actions Linux",
              unitType: "minutes",
              pricePerUnit: 0.008,
              grossQuantity: 100,
              grossAmount: 0.8,
              discountAmount: 0,
              netQuantity: 100,
              netAmount: 0.8,
            },
          ],
        }),
      "GET /api/v3/users/admin/settings/billing/ai_credit/usage": () =>
        jsonResponse({ timePeriod: { year: 2026, month: 8 }, user: "admin", usageItems: [] }),
      "GET /api/v3/users/admin/settings/billing/premium_request/usage": () =>
        jsonResponse({ timePeriod: { year: 2026, month: 8 }, user: "admin", usageItems: [] }),
    });
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: "Billing and plans" }));

    await waitFor(() => expect(screen.getByText("Actions Linux")).toBeInTheDocument());
    expect(screen.getByText("actions")).toBeInTheDocument();
    // netAmount 0.8 rendered as currency.
    expect(screen.getAllByText("$0.80").length).toBeGreaterThan(0);

    const summaryGet = mockFetch.mock.calls.find(
      (c) =>
        String(c[0]).includes("/users/admin/settings/billing/usage/summary") &&
        ((c[1] as RequestInit | undefined)?.method ?? "GET") === "GET",
    );
    expect(summaryGet).toBeDefined();
  });

  it("opens the tab named by ?tab= so settings sections are deep-linkable", async () => {
    installFetchRoutes();
    renderPage("/ui/account?tab=emails");
    expect(screen.getByRole("button", { name: "Emails" })).toHaveAttribute("aria-current", "page");
    await waitFor(() => screen.getByText("admin@example.com"));
    fireEvent.click(screen.getByRole("button", { name: "Appearance" }));
    expect(screen.getByRole("button", { name: "Appearance" })).toHaveAttribute("aria-current", "page");
    expect(await screen.findByRole("radio", { name: /sync with system/i })).toBeInTheDocument();
  });

  it("falls back to Public profile for an unknown ?tab= value", async () => {
    installFetchRoutes();
    renderPage("/ui/account?tab=does-not-exist");
    expect(screen.getByRole("button", { name: "Public profile" })).toHaveAttribute("aria-current", "page");
  });

  it("offers Light / Dark / Sync with system in Appearance", async () => {
    installFetchRoutes();
    // Mount with a persisted explicit override so "system" is not preselected.
    window.localStorage.setItem("bleephub:theme", "dark");
    renderPage("/ui/account?tab=appearance");
    expect(await screen.findByRole("radio", { name: /^Light/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^Dark/ })).toBeChecked();
    fireEvent.click(screen.getByRole("radio", { name: /sync with system/i }));
    // System mode clears the persisted override.
    expect(window.localStorage.getItem("bleephub:theme")).toBe(null);
  });

  it("sets a verified address as primary via PUT /ui-data/user/emails/primary", async () => {
    installFetchRoutes({
      "GET /api/v3/user/emails": () =>
        jsonResponse([
          { email: "admin@example.com", primary: true, verified: true, visibility: "private" },
          { email: "alt@example.com", primary: false, verified: true, visibility: null },
          { email: "unverified@example.com", primary: false, verified: false, visibility: null },
        ]),
      "PUT /ui-data/user/emails/primary": () =>
        jsonResponse([
          { email: "alt@example.com", primary: true, verified: true, visibility: "private" },
          { email: "admin@example.com", primary: false, verified: true, visibility: "private" },
        ]),
    });
    renderPage("/ui/account?tab=emails");
    await waitFor(() => screen.getByText("alt@example.com"));
    // Only verified non-primary addresses get the action.
    expect(screen.queryByRole("button", { name: "Set unverified@example.com as primary" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Set alt@example.com as primary" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/ui-data/user/emails/primary" && (c[1] as RequestInit | undefined)?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(String((put![1] as RequestInit).body)).toContain("alt@example.com");
    });
  });

  it("lists, creates, and deletes classic tokens via /api/v3/authorizations", async () => {
    installFetchRoutes({
      "GET /api/v3/authorizations": () =>
        jsonResponse([
          {
            id: 42, url: "/api/v3/authorizations/42", scopes: ["repo"], token: "",
            token_last_eight: "abcd1234", hashed_token: "h",
            app: { client_id: "00000000000000000000", name: "GitHub API", url: "https://github.com" },
            note: "ci token", note_url: null, fingerprint: null,
            created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z", expires_at: null,
          },
        ]),
      "POST /ui-data/user/tokens/classic": () =>
        jsonResponse({
          id: 43, url: "/api/v3/authorizations/43", scopes: ["repo", "read:org"], token: "ghp_shown_once",
          token_last_eight: "wxyz9876", hashed_token: "h2",
          app: { client_id: "00000000000000000000", name: "GitHub API", url: "https://github.com" },
          note: "new one", note_url: null, fingerprint: null,
          created_at: "2026-08-19T00:00:00Z", updated_at: "2026-08-19T00:00:00Z", expires_at: "2026-09-19T23:59:59Z",
        }, 201),
      "DELETE /api/v3/authorizations/42": () => new Response(null, { status: 204 }),
    });
    renderPage("/ui/account?tab=tokens");
    expect(await screen.findByText("ci token")).toBeInTheDocument();
    expect(screen.getByText(/…abcd1234/)).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "new one" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /repo Full control of private repositories/ }));
    fireEvent.click(screen.getByRole("button", { name: "Generate classic token" }));
    expect(await screen.findByText("ghp_shown_once")).toBeInTheDocument();
    // Create via the browser-only /ui-data endpoint — legacy /api/v3/authorizations takes no expiration.
    const post = mockFetch.mock.calls.find(
      (c) => String(c[0]) === "/ui-data/user/tokens/classic" && (c[1] as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeDefined();
    const body = JSON.parse(String((post![1] as RequestInit).body));
    expect(body).toMatchObject({ note: "new one", scopes: ["repo"] });
    // The default preset (30 days) posts a concrete RFC 3339 expires_at.
    expect(body.expires_at).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/);

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/authorizations/42" && (c[1] as RequestInit | undefined)?.method === "DELETE",
      );
      expect(del).toBeDefined();
    });
  });

  it("computes the fine-grained expiration from the preset and warns on no expiration", async () => {
    installFetchRoutes();
    renderPage("/ui/account?tab=tokens");
    // Two expiration pickers render: fine-grained first, classic second; wait for the classic form.
    await screen.findByText("New classic token");
    const presets = await screen.findAllByLabelText("Expiration");
    expect(presets).toHaveLength(2);
    const preset = presets[0]!;
    // Default 30 days: the computed date is displayed (by both pickers).
    expect((preset as HTMLSelectElement).value).toBe("30");
    expect(screen.getAllByText(/The token will expire on/).length).toBeGreaterThan(0);
    fireEvent.change(preset, { target: { value: "none" } });
    expect(screen.getByText(/This token will never expire/)).toBeInTheDocument();
    fireEvent.change(preset, { target: { value: "custom" } });
    expect(screen.getByLabelText("Custom expiration date")).toBeInTheDocument();
  });

  it("posts null expires_at when a classic token is created with No expiration", async () => {
    installFetchRoutes({
      "POST /ui-data/user/tokens/classic": () =>
        jsonResponse({
          id: 44, url: "/api/v3/authorizations/44", scopes: [], token: "ghp_eternal",
          token_last_eight: "eeee8888", hashed_token: "h3",
          app: { client_id: "00000000000000000000", name: "GitHub API", url: "https://github.com" },
          note: "forever", note_url: null, fingerprint: null,
          created_at: "2026-08-19T00:00:00Z", updated_at: "2026-08-19T00:00:00Z", expires_at: null,
        }, 201),
    });
    renderPage("/ui/account?tab=tokens");
    await screen.findByText("New classic token");
    const presets = await screen.findAllByLabelText("Expiration");
    expect(presets).toHaveLength(2);
    const classicPreset = presets[1]!;
    fireEvent.change(classicPreset, { target: { value: "none" } });
    expect(screen.getByText(/This token will never expire/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Note"), { target: { value: "forever" } });
    fireEvent.click(screen.getByRole("button", { name: "Generate classic token" }));
    expect(await screen.findByText("ghp_eternal")).toBeInTheDocument();
    const post = mockFetch.mock.calls.find(
      (c) => String(c[0]) === "/ui-data/user/tokens/classic" && (c[1] as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeDefined();
    const body = JSON.parse(String((post![1] as RequestInit).body));
    expect(body.expires_at).toBeNull();
  });

  it("renames the account via PATCH /api/v3/admin/users/{username} when the viewer is a site admin", async () => {
    installFetchRoutes({
      "PATCH /api/v3/admin/users/admin": () =>
        jsonResponse({ message: "Job queued to rename user.", url: "/user/1" }, 202),
    });
    renderPage("/ui/account?tab=account");
    const input = await screen.findByLabelText("New username");
    fireEvent.change(input, { target: { value: "root" } });
    fireEvent.click(screen.getByRole("button", { name: "Change username" }));
    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/admin/users/admin" && (c[1] as RequestInit | undefined)?.method === "PATCH",
      );
      expect(patch).toBeDefined();
      expect(JSON.parse(String((patch![1] as RequestInit).body))).toEqual({ login: "root" });
    });
  });

  it("shows a contact-your-administrator note instead of rename/delete for non-admins", async () => {
    installFetchRoutes({
      "GET /api/v3/user": () =>
        jsonResponse({ id: 2, login: "mona", type: "User", site_admin: false }),
    });
    renderPage("/ui/account?tab=account");
    expect(await screen.findByText(/Contact your administrator/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change username" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete your account" })).toBeNull();
  });

  it("deletes the account after typed confirmation, then leaves via the sign-out form", async () => {
    const submitSpy = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(() => {});
    installFetchRoutes({
      "DELETE /api/v3/admin/users/admin": () => new Response(null, { status: 204 }),
    });
    renderPage("/ui/account?tab=account");
    fireEvent.click(await screen.findByRole("button", { name: "Delete your account" }));
    const confirmInput = await screen.findByLabelText(/To confirm, type/);
    const deleteBtn = screen.getByRole("button", { name: "Delete this account" });
    expect(deleteBtn).toBeDisabled();
    fireEvent.change(confirmInput, { target: { value: "admin" } });
    expect(deleteBtn).not.toBeDisabled();
    fireEvent.click(deleteBtn);
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/admin/users/admin" && (c[1] as RequestInit | undefined)?.method === "DELETE",
      );
      expect(del).toBeDefined();
      expect(submitSpy).toHaveBeenCalled();
    });
    submitSpy.mockRestore();
  });

  it("renders the Applications tab with the shared authorized-applications list", async () => {
    installFetchRoutes({
      "GET /settings/connections/applications": () =>
        jsonResponse([{ client_id: "c1", name: "Example App", type: "OAuthApp", url: "", scopes: ["repo"], created_at: "2026-01-01T00:00:00Z" }]),
    });
    renderPage("/ui/account?tab=applications");
    expect(await screen.findByText("Example App")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Revoke" })).toBeInTheDocument();
  });
});
