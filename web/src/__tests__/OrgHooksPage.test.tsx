import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgHooksPage } from "../pages/OrgHooksPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

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

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/orgs/acme/hooks"]}>
        <Routes>
          <Route path="/ui/orgs/:org/hooks" element={<OrgHooksPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const orgHook = {
  id: 3,
  type: "Organization",
  name: "web",
  active: true,
  events: ["push", "issues"],
  config: { url: "https://ci.example.test/org-hook", content_type: "json", insecure_ssl: "0" },
  created_at: "2026-05-01T00:00:00Z",
  updated_at: "2026-05-01T00:00:00Z",
  url: "/api/v3/orgs/acme/hooks/3",
  ping_url: "/api/v3/orgs/acme/hooks/3/pings",
  deliveries_url: "/api/v3/orgs/acme/hooks/3/deliveries",
};

describe("OrgHooksPage", () => {
  it("lists organization webhooks with a deliveries link", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) {
        return Promise.resolve(jsonResponse([orgHook]));
      }
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("#3")).toBeInTheDocument());
    expect(screen.getByText(/https:\/\/ci\.example\.test\/org-hook/)).toBeInTheDocument();
    expect(screen.getByText(/events: push, issues/)).toBeInTheDocument();
    expect(mockFetch).toHaveBeenCalledWith(
      "/api/v3/orgs/acme/hooks?per_page=30",
      expect.anything(),
    );

    const link = screen.getByRole("link", { name: /deliveries/i });
    expect(link).toHaveAttribute("href", "/ui/orgs/acme/hooks/3/deliveries");
  });

  it("shows an honest empty state when the org has no webhooks", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) {
        return Promise.resolve(jsonResponse([]));
      }
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    await waitFor(() =>
      expect(screen.getByText("No organization webhooks")).toBeInTheDocument(),
    );
  });

  it("surfaces list errors instead of swallowing them", async () => {
    mockFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse({ message: "boom" }, 500)),
    );
    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/failed to load organization webhooks/i)).toBeInTheDocument(),
    );
  });
});

describe("OrgHooksPage write actions", () => {
  it("creates a webhook", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ ...orgHook, id: 9 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /new webhook/i }));
    fireEvent.change(await screen.findByLabelText(/payload url/i), {
      target: { value: "https://x.test/h" },
    });
    fireEvent.click(screen.getByRole("button", { name: /add webhook/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/hooks") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      const body = JSON.parse((post![1] as RequestInit).body as string);
      expect(body.config.url).toBe("https://x.test/h");
      expect(body.events).toEqual(["push"]);
      // SSL verification defaults to enabled.
      expect(body.config.insecure_ssl).toBe("0");
    });
  });

  it("creates a webhook with SSL verification disabled", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ ...orgHook, id: 11 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /new webhook/i }));
    fireEvent.change(await screen.findByLabelText(/payload url/i), {
      target: { value: "https://x.test/h" },
    });
    expect(screen.getByRole("radio", { name: "Enable SSL verification" })).toBeChecked();
    fireEvent.click(screen.getByRole("radio", { name: "Disable (not recommended)" }));
    expect(
      screen.getByText("Warning: SSL certificates will not be verified when delivering payloads."),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /add webhook/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/hooks") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      const body = JSON.parse((post![1] as RequestInit).body as string);
      expect(body.config.insecure_ssl).toBe("1");
    });
  });

  it("pings a webhook", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks/3/pings") && init?.method === "POST") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([orgHook]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /ping webhook 3/i }));
    await waitFor(() => {
      expect(
        mockFetch.mock.calls.find(
          (c) => String(c[0]).endsWith("/hooks/3/pings") && c[1]?.method === "POST",
        ),
      ).toBeTruthy();
    });
  });

  it("creates a webhook with a secret and individual events", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ ...orgHook, id: 10 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /new webhook/i }));
    fireEvent.change(await screen.findByLabelText(/payload url/i), {
      target: { value: "https://x.test/h" },
    });
    fireEvent.change(screen.getByLabelText(/secret/i), { target: { value: "org-s3cret" } });
    fireEvent.click(screen.getByRole("radio", { name: "Let me select individual events" }));
    // "push" is pre-selected (it seeds the individual list); add "organization".
    expect(screen.getByRole("checkbox", { name: "push" })).toBeChecked();
    fireEvent.click(screen.getByRole("checkbox", { name: "organization" }));
    fireEvent.click(screen.getByRole("button", { name: /add webhook/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/hooks") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      const body = JSON.parse((post![1] as RequestInit).body as string);
      expect(body.config.secret).toBe("org-s3cret");
      expect(body.events).toEqual(["organization", "push"]);
    });
  });

  it("edits a webhook via PATCH, keeping the stored secret when left blank", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks/3") && init?.method === "PATCH") {
        return Promise.resolve(new Response(null, { status: 200 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([orgHook]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /edit webhook 3/i }));

    const urlInput = await screen.findByLabelText(/payload url/i);
    expect(urlInput).toHaveValue("https://ci.example.test/org-hook");
    // push+issues → individual-events mode, prechecked.
    expect(screen.getByRole("radio", { name: "Let me select individual events" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "issues" })).toBeChecked();

    fireEvent.change(urlInput, { target: { value: "https://ci.example.test/v2" } });
    fireEvent.click(screen.getByRole("button", { name: /update webhook/i }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/hooks/3") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      const body = JSON.parse((patch![1] as RequestInit).body as string);
      expect(body).toEqual({
        active: true,
        events: ["issues", "push"],
        config: { url: "https://ci.example.test/v2", content_type: "json", insecure_ssl: "0" },
      });
      // Blank secret is omitted so the server keeps the stored one.
      expect(body.config.secret).toBeUndefined();
    });
  });

  it("pre-selects Disable when editing a hook whose config has insecure_ssl \"1\"", async () => {
    const insecureHook = { ...orgHook, config: { ...orgHook.config, insecure_ssl: "1" } };
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks/3") && init?.method === "PATCH") {
        return Promise.resolve(new Response(null, { status: 200 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([insecureHook]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /edit webhook 3/i }));

    expect(await screen.findByRole("radio", { name: "Disable (not recommended)" })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: /update webhook/i }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/hooks/3") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      const body = JSON.parse((patch![1] as RequestInit).body as string);
      expect(body.config.insecure_ssl).toBe("1");
    });
  });

  it("deletes a webhook after confirmation", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks/3") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([orgHook]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /delete webhook 3/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^delete$/i }));
    await waitFor(() => {
      expect(
        mockFetch.mock.calls.find(
          (c) => String(c[0]).endsWith("/hooks/3") && c[1]?.method === "DELETE",
        ),
      ).toBeTruthy();
    });
  });
});
