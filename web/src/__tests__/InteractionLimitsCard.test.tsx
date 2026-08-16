import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { InteractionLimitsCard } from "../components/InteractionLimitsCard.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderCard(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <InteractionLimitsCard path={path} queryKey={["il", path]} scopeLabel="everywhere" />
    </QueryClientProvider>,
  );
}

describe("InteractionLimitsCard", () => {
  it("reflects the current limit and PUTs a new one with an expiry", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/v3/user/interaction-limits" && (init?.method ?? "GET") === "GET") {
        return Promise.resolve(jsonResponse({ limit: "existing_users" }));
      }
      return Promise.resolve(new Response(null, { status: 204 }));
    });
    renderCard("/api/v3/user/interaction-limits");

    const select = await screen.findByRole("combobox", { name: "Interaction limit" });
    await waitFor(() => expect((select as HTMLSelectElement).value).toBe("existing_users"));

    fireEvent.change(select, { target: { value: "collaborators_only" } });
    fireEvent.click(screen.getByRole("button", { name: "Set limit" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/user/interaction-limits" && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(JSON.parse(String(put![1].body))).toEqual({ limit: "collaborators_only", expiry: "one_month" });
    });
  });

  it("DELETEs when Clear limit is pressed", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/interaction-limits") && (init?.method ?? "GET") === "GET") {
        return Promise.resolve(jsonResponse({}));
      }
      return Promise.resolve(new Response(null, { status: 204 }));
    });
    renderCard("/api/v3/orgs/acme/interaction-limits");

    fireEvent.click(await screen.findByRole("button", { name: "Clear limit" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/orgs/acme/interaction-limits" && c[1]?.method === "DELETE",
      );
      expect(del).toBeDefined();
    });
  });
});
