import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { OAuthPage } from "../pages/OAuthPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown) {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
  vi.restoreAllMocks();
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <OAuthPage />
    </QueryClientProvider>,
  );
}

describe("OAuthPage", () => {
  it("renders OAuth flow controls and loads registered clients", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/settings/apps" || url === "/settings/oauth-apps" || url === "/settings/connections/applications") {
        return Promise.resolve(jsonResponse([]));
      }
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    expect(screen.getAllByText(/OAuth flow controls/i).length).toBeGreaterThan(0);
    expect(await screen.findByLabelText("Registered application")).toBeInTheDocument();
    expect(mockFetch).toHaveBeenCalledTimes(3);
  });

  it("starts device flow through the GitHub device-code endpoint", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/settings/apps" || url === "/settings/oauth-apps" || url === "/settings/connections/applications") return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ device_code: "device-123", user_code: "ABCD-EFGH" }));
    });
    renderPage();
    fireEvent.change(screen.getByLabelText("Client identifier"), { target: { value: "Iv1.client" } });
    fireEvent.click(screen.getByRole("button", { name: "Device flow" }));

    await waitFor(() => {
      expect(mockFetch.mock.calls.some(([url]) => url === "/login/device/code")).toBe(true);
    });
    const [url, opts] = mockFetch.mock.calls.find(([calledUrl]) => calledUrl === "/login/device/code")!;
    expect(url).toBe("/login/device/code");
    expect(opts).toMatchObject({ method: "POST" });
    const body = new URLSearchParams(String(opts.body));
    expect(body.get("client_id")).toBe("Iv1.client");
    expect(body.get("scope")).toBe("repo read:org");
    expect(screen.getByText(/ABCD-EFGH/)).toBeInTheDocument();
  });

  it("polls the shared OAuth access-token endpoint for a device token", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/settings/apps" || url === "/settings/oauth-apps" || url === "/settings/connections/applications") return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ access_token: "gho_token", token_type: "bearer", scope: "repo" }));
    });
    renderPage();
    fireEvent.change(screen.getByLabelText("Client identifier"), { target: { value: "Iv1.client" } });
    fireEvent.change(screen.getByLabelText("Device code"), { target: { value: "device-123" } });
    fireEvent.click(screen.getByRole("button", { name: "Poll device token" }));

    await waitFor(() => {
      expect(mockFetch.mock.calls.some(([url]) => url === "/login/oauth/access_token")).toBe(true);
    });
    const [url, opts] = mockFetch.mock.calls.find(([calledUrl]) => calledUrl === "/login/oauth/access_token")!;
    expect(url).toBe("/login/oauth/access_token");
    expect(opts).toMatchObject({ method: "POST" });
    expect(opts.headers).toMatchObject({ Accept: "application/json" });
    const body = new URLSearchParams(String(opts.body));
    expect(body.get("client_id")).toBe("Iv1.client");
    expect(body.get("device_code")).toBe("device-123");
  });

  it("opens the GitHub OAuth authorize endpoint for web flow", () => {
    mockFetch.mockImplementation(() => Promise.resolve(jsonResponse([])));
    // window.open must omit "noopener": it makes browsers return null, misread as "blocked".
    const openSpy = vi.spyOn(window, "open").mockReturnValue({} as Window);
    renderPage();
    fireEvent.change(screen.getByLabelText("Client identifier"), { target: { value: "Iv1.client" } });
    fireEvent.click(screen.getByRole("button", { name: "Web flow" }));

    expect(openSpy).toHaveBeenCalledWith(
      "/login/oauth/authorize?client_id=Iv1.client&redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fcallback&scope=repo%20read%3Aorg&state=STATE-1",
      "_blank",
    );
    expect(screen.getByText(/Opened .* in a new tab\./)).toBeInTheDocument();
    expect(screen.queryByText(/blocked the OAuth window/)).not.toBeInTheDocument();
  });

  it("lists and revokes an authorized OAuth grant", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/settings/apps" || url === "/settings/oauth-apps") return Promise.resolve(jsonResponse([]));
      if (url === "/settings/connections/applications" && init?.method !== "DELETE") {
        return Promise.resolve(jsonResponse([{
          client_id: "oauth-client",
          name: "Example OAuth",
          type: "OAuthApp",
          url: "https://example.test",
          scopes: ["repo"],
          created_at: "2026-01-01T00:00:00Z",
        }]));
      }
      if (url === "/settings/connections/applications/oauth-client") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();

    expect(await screen.findByText("Example OAuth")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/settings/connections/applications/oauth-client",
        expect.objectContaining({ method: "DELETE" }),
      );
    });
  });
});
