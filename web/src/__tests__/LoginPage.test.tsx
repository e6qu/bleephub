import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { LoginPage } from "../pages/LoginPage.js";
import { clearToken, getToken } from "../api.js";

vi.mock("../components/Shell.js", () => ({ BleephubBuildFooter: () => null }));

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

// jsdom can't navigate; stub window.location.href writable to assert on it.
const originalLocation = window.location;
beforeEach(() => {
  const stub = { ...originalLocation, href: "" };
  Object.defineProperty(window, "location", { value: stub, writable: true, configurable: true });
});

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
  clearToken();
  Object.defineProperty(window, "location", {
    value: originalLocation,
    writable: true,
    configurable: true,
  });
});

async function submitToken(token: string) {
  render(<LoginPage />);
  fireEvent.change(await screen.findByLabelText(/access token/i), { target: { value: token } });
  fireEvent.click(screen.getByRole("button", { name: /sign in/i }));
}

describe("LoginPage", () => {
  it("exposes the same-origin Shauth starter without rendering legacy credentials", async () => {
    mockFetch.mockResolvedValueOnce(new Response(JSON.stringify({ shauth: true }), { status: 200 }));
    render(<LoginPage />);
    await waitFor(() => {
      expect(window.location.href).toBe("/auth/shauth?return_to=%2Fui%2F");
    });
    expect(screen.getByRole("heading", { name: "Sign in to Bleephub" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in with Shauth" })).toHaveAttribute(
      "href",
      "/auth/shauth?return_to=%2Fui%2F",
    );
    expect(screen.queryByLabelText(/access token/i)).not.toBeInTheDocument();
  });

  it("exchanges the token for an HttpOnly browser session and retains no bearer", async () => {
    mockFetch
      .mockResolvedValueOnce(new Response(JSON.stringify({ github: true }), { status: 200 }))
      .mockResolvedValue(new Response(JSON.stringify({ login: "octocat" }), { status: 200 }));
    await submitToken("ghp_validpat");
    await waitFor(() => {
      expect(window.location.href).toBe("/ui/");
    });
    const [url, opts] = mockFetch.mock.calls.find(([url]) => url.toString() === "/auth/token")!;
    expect(url.toString()).toBe("/auth/token");
    expect((opts.headers as Record<string, string>).Authorization).toBe("Bearer ghp_validpat");
    expect(getToken()).toBeNull();
  });

  it("accepts an OAuth token when GitHub REST identity accepts it", async () => {
    mockFetch
      .mockResolvedValueOnce(new Response(JSON.stringify({ github: true }), { status: 200 }))
      .mockResolvedValue(new Response(JSON.stringify({ login: "octocat" }), { status: 200 }));
    await submitToken("gho_oauthtoken");
    await waitFor(() => {
      expect(window.location.href).toBe("/ui/");
    });
    expect(mockFetch.mock.calls.some(([url]) => url.toString() === "/auth/token")).toBe(true);
    expect(getToken()).toBeNull();
  });

  it("an instance with no providers configured shows the token form and no error", async () => {
    mockFetch.mockResolvedValueOnce(new Response(JSON.stringify({}), { status: 200 }));
    render(<LoginPage />);
    expect(await screen.findByLabelText(/access token/i)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Continue with GitHub/i })).not.toBeInTheDocument();
  });

  it("an unreachable provider list is distinguished from having no providers", async () => {
    mockFetch.mockRejectedValueOnce(new TypeError("Failed to fetch"));
    render(<LoginPage />);

    // Token sign-in stays offered, but the failure reason must be stated, not look like "none configured".
    expect(await screen.findByLabelText(/access token/i)).toBeInTheDocument();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/Could not load the available sign-in methods/i);
    expect(alert).toHaveTextContent(/Failed to fetch/);
  });

  it("a provider list that answers non-2xx is also reported, not treated as empty", async () => {
    mockFetch.mockResolvedValueOnce(
      new Response("nope", { status: 503, statusText: "Service Unavailable" }),
    );
    render(<LoginPage />);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/503/);
  });

  it("rejects a token when GitHub REST identity rejects it", async () => {
    mockFetch
      .mockResolvedValueOnce(new Response(JSON.stringify({ github: true }), { status: 200 }))
      .mockResolvedValue(new Response(JSON.stringify({ message: "Requires authentication" }), { status: 401 }));
    await submitToken("bad-token");
    await waitFor(() => {
      expect(screen.getByText(/active user access token/i)).toBeInTheDocument();
    });
    expect(window.location.href).toBe("");
    expect(getToken()).toBeNull();
  });
});
