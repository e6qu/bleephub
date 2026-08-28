import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { SecretScanningPage } from "../pages/SecretScanningPage.js";

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
      <MemoryRouter initialEntries={["/ui/admin/secret-repo/security/secret-scanning"]}>
        <Routes>
          <Route
            path="/ui/:owner/:repo/security/secret-scanning"
            element={<SecretScanningPage />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const patternsPath = "/api/v3/repos/admin/secret-repo/secret-scanning/custom-patterns";

const existingPattern = {
  id: 42,
  name: "Acme API key",
  pattern: "acme_[0-9a-f]{32}",
  slug: "acme-api-key",
  state: "published",
  custom_pattern_version: "v-abc",
};

describe("SecretScanningPage custom patterns", () => {
  it("creates a pattern via POST .../custom-patterns with the name", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u === patternsPath && opts?.method === "POST") {
        return Promise.resolve(jsonResponse({ created_patterns: [existingPattern] }, 201));
      }
      if (u === patternsPath) return Promise.resolve(jsonResponse([]));
      // Alerts list and open-count (issues/pulls) endpoints all return arrays.
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();

    await screen.findByRole("heading", { name: /custom patterns/i });

    fireEvent.change(screen.getByLabelText("Pattern name"), { target: { value: "Acme API key" } });
    fireEvent.change(screen.getByLabelText("Secret format (regex)"), {
      target: { value: "acme_[0-9a-f]{32}" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create pattern" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0] === patternsPath && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      const body = JSON.parse(String(post![1].body));
      expect(body).toEqual({ patterns: [{ name: "Acme API key", pattern: "acme_[0-9a-f]{32}" }] });
    });
  });

  it("deletes a pattern via DELETE .../custom-patterns referencing its id", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u === patternsPath && opts?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u === patternsPath) return Promise.resolve(jsonResponse([existingPattern]));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();

    await screen.findByText("Acme API key");

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    // The modal's confirm button is also labelled "Delete"; take the last one.
    const confirmButtons = await screen.findAllByRole("button", { name: "Delete" });
    fireEvent.click(confirmButtons[confirmButtons.length - 1]!);

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0] === patternsPath && c[1]?.method === "DELETE",
      );
      expect(del).toBeDefined();
      const body = JSON.parse(String(del![1].body));
      expect(body.patterns[0].pattern_id).toBe(42);
    });
  });
});
