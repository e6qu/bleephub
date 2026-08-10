import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AuditLogPage, auditEventsToCsv } from "../pages/AuditLogPage.js";
import type { BleephubAuditEvent } from "../types.js";

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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <AuditLogPage />
    </QueryClientProvider>,
  );
}

const event: BleephubAuditEvent = {
  id: 1,
  created_at: "2026-01-01T00:00:00Z",
  actor_login: "admin",
  action: "create_user",
  entity_type: "user",
  entity_id: "5",
  details: { a: 1 },
};

describe("auditEventsToCsv", () => {
  it("emits a header row and JSON-encodes object fields", () => {
    const [header, row] = auditEventsToCsv([event]).split("\n");
    expect(header).toBe("id,created_at,actor_login,action,entity_type,entity_id,details");
    expect(row).toContain('"create_user"');
    expect(row).toContain('"{""a"":1}"');
  });

  it("escapes embedded quotes and commas", () => {
    const csv = auditEventsToCsv([{ ...event, action: 'a,"b' }]);
    expect(csv).toContain('"a,""b"');
  });
});

describe("AuditLogPage export", () => {
  beforeEach(() => {
    vi.stubGlobal("URL", {
      ...URL,
      createObjectURL: vi.fn(() => "blob:x"),
      revokeObjectURL: vi.fn(),
    });
  });

  it("downloads CSV of the loaded events when Export CSV is clicked", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/organizations")) return Promise.resolve(jsonResponse([{ login: "acme", id: 1 }]));
      if (u.includes("/audit-log")) {
        return Promise.resolve(
          jsonResponse([
            {
              _document_id: "1",
              actor: "admin",
              action: "create_user",
              "@timestamp": "2026-01-01T00:00:00Z",
              org: "acme",
              data: { entity_type: "user", entity_id: 5 },
            },
          ]),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    const csvButton = await screen.findByRole("button", { name: /export csv/i });
    await waitFor(() => expect(csvButton).toBeEnabled());
    fireEvent.click(csvButton);
    expect(URL.createObjectURL).toHaveBeenCalledOnce();
  });
});
