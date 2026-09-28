import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { fetchCurrentUser } from "../api.js";
import { AccountPage } from "../pages/AccountPage.js";

// Deleting the account signs the page out for good, so this test has its own
// file and module instance; AccountPage.test.tsx covers the rest of the page.

const mockFetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
  const key = `${init?.method ?? "GET"} ${String(input)}`;
  if (key === "GET /api/v3/user") return Promise.resolve(jsonResponse({ id: 1, login: "admin", type: "User", site_admin: true }));
  if (key === "DELETE /api/v3/admin/users/admin") return Promise.resolve(new Response(null, { status: 204 }));
  return Promise.resolve(jsonResponse({ message: `no route for ${key}` }, 404));
});
globalThis.fetch = mockFetch;

Object.defineProperty(window, "matchMedia", {
  configurable: true,
  value: vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
});

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

describe("account deletion", () => {
  it("deletes after typed confirmation, then signs out and leaves via the sign-out form", async () => {
    const submitSpy = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <MemoryRouter initialEntries={["/ui/account?tab=account"]}>
          <AccountPage />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Delete your account" }));
    const confirmInput = await screen.findByLabelText(/To confirm, type/);
    const deleteBtn = screen.getByRole("button", { name: "Delete this account" });
    expect(deleteBtn).toBeDisabled();
    fireEvent.change(confirmInput, { target: { value: "admin" } });
    expect(deleteBtn).not.toBeDisabled();
    fireEvent.click(deleteBtn);
    await waitFor(() => expect(submitSpy).toHaveBeenCalled());
    expect(
      mockFetch.mock.calls.some(([u, i]) => String(u) === "/api/v3/admin/users/admin" && i?.method === "DELETE"),
    ).toBe(true);

    // Nothing may reach the network while the sign-out navigation runs.
    const sent = mockFetch.mock.calls.length;
    await expect(fetchCurrentUser()).rejects.toThrow();
    expect(mockFetch).toHaveBeenCalledTimes(sent);
    submitSpy.mockRestore();
  });
});
