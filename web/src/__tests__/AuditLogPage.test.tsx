import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AuditLogPage, auditEventsToCsv, parseAuditQuery, filterByCreated } from "../pages/AuditLogPage.js";
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

describe("parseAuditQuery", () => {
  it("extracts action:/actor:/created: qualifiers and keeps free text", () => {
    expect(parseAuditQuery("action:repo.create actor:admin created:2026-01-01..2026-02-01 settings page")).toEqual({
      action: "repo.create",
      actor: "admin",
      from: "2026-01-01",
      to: "2026-02-01",
      text: "settings page",
    });
  });

  it("supports >= and <= created bounds and single-day filters", () => {
    expect(parseAuditQuery("created:>=2026-03-01")).toMatchObject({ from: "2026-03-01" });
    expect(parseAuditQuery("created:<=2026-03-31")).toMatchObject({ to: "2026-03-31" });
    expect(parseAuditQuery("created:2026-03-15")).toMatchObject({ from: "2026-03-15", to: "2026-03-15" });
  });

  it("passes plain text through untouched", () => {
    expect(parseAuditQuery("just some words")).toEqual({ text: "just some words" });
  });
});

describe("filterByCreated", () => {
  const at = (iso: string): BleephubAuditEvent => ({ ...event, created_at: iso });
  it("keeps events inside the inclusive range", () => {
    const events = [at("2026-01-01T05:00:00Z"), at("2026-02-15T00:00:00Z"), at("2026-03-01T00:00:00Z")];
    expect(filterByCreated(events, "2026-01-01", "2026-02-28")).toHaveLength(2);
    expect(filterByCreated(events, undefined, "2026-01-31")).toHaveLength(1);
    expect(filterByCreated(events, "2026-03-01", undefined)).toHaveLength(1);
    expect(filterByCreated(events)).toHaveLength(3);
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

  it("walks the audit-log pages for the current filters when Export CSV is clicked", async () => {
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
    await waitFor(() => expect(URL.createObjectURL).toHaveBeenCalledOnce());
    // The export refetches with explicit pagination instead of reusing loaded rows.
    const exportCall = mockFetch.mock.calls.find((c) => String(c[0]).includes("page=1"));
    expect(exportCall).toBeDefined();
    expect(String(exportCall![0])).toContain("per_page=100");
  });
});
