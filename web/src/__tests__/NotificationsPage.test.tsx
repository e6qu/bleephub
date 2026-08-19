import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { NotificationsPage } from "../pages/NotificationsPage.js";

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
  // The group-by-repo choice persists in localStorage; isolate the tests.
  localStorage.clear();
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <NotificationsPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const thread = {
  id: "t1",
  repository: { full_name: "admin/repo" },
  subject: { title: "Issue title", url: "/api/v3/repos/admin/repo/issues/1", latest_comment_url: "", type: "Issue" },
  reason: "subscribed",
  unread: true,
  updated_at: "2026-01-01T00:00:00Z",
  last_read_at: null,
  subscription_url: "/api/v3/notifications/threads/t1/subscription",
  url: "/api/v3/notifications/threads/t1",
};

function mockEndpoints({
  saved = [] as unknown[],
  done = [] as unknown[],
} = {}) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url.startsWith("/ui-data/notifications/threads/") && url.endsWith("/saved")) {
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (url.startsWith("/ui-data/notifications?view=saved")) {
      return Promise.resolve(jsonResponse(saved));
    }
    if (url.startsWith("/ui-data/notifications?view=done")) {
      return Promise.resolve(jsonResponse(done));
    }
    if (url.split("?")[0]! === "/api/v3/notifications") {
      return Promise.resolve(jsonResponse([thread]));
    }
    if (url === "/api/v3/notifications/threads/t1/subscription") {
      if (init?.method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(
        jsonResponse({
          subscribed: true,
          ignored: false,
          reason: "subscribed",
          created_at: "2026-01-01T00:00:00Z",
          url: "/api/v3/notifications/threads/t1/subscription",
          thread_url: "/api/v3/notifications/threads/t1/subscription",
        }),
      );
    }
    if (url === "/api/v3/notifications/threads/t1" && init?.method === "PATCH") {
      return Promise.resolve(new Response(null, { status: 205 }));
    }
    if (url === "/api/v3/notifications/threads/t1" && init?.method === "DELETE") {
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (url === "/api/v3/notifications" && init?.method === "PUT") {
      return Promise.resolve(new Response(null, { status: 202 }));
    }
    return Promise.resolve(jsonResponse({}));
  });
}

describe("NotificationsPage", () => {
  it("renders unread notifications", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Notifications")).toBeInTheDocument();
      expect(screen.getByText("Issue title")).toBeInTheDocument();
    });
  });

  it("filters the inbox by repository and by reason", async () => {
    const t2 = {
      ...thread,
      id: "t2",
      repository: { full_name: "octo/other" },
      subject: { ...thread.subject, title: "Other issue", url: "/api/v3/repos/octo/other/issues/2" },
      reason: "mention",
      url: "/api/v3/notifications/threads/t2",
    };
    mockFetch.mockImplementation((url: string) => {
      if (url.split("?")[0]! === "/api/v3/notifications") {
        return Promise.resolve(jsonResponse([thread, t2]));
      }
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("Other issue")).toBeInTheDocument());
    expect(screen.getByText("Issue title")).toBeInTheDocument();

    // Filter by repository → only octo/other's thread remains.
    fireEvent.change(screen.getByLabelText("Filter by repository"), { target: { value: "octo/other" } });
    expect(screen.getByText("Other issue")).toBeInTheDocument();
    expect(screen.queryByText("Issue title")).toBeNull();

    // Reset repo, filter by reason → only the 'subscribed' thread remains.
    fireEvent.change(screen.getByLabelText("Filter by repository"), { target: { value: "" } });
    fireEvent.change(screen.getByLabelText("Filter by reason"), { target: { value: "subscribed" } });
    expect(screen.getByText("Issue title")).toBeInTheDocument();
    expect(screen.queryByText("Other issue")).toBeNull();
  });

  it("groups notifications under per-repository headers", async () => {
    const t2 = {
      ...thread,
      id: "t2",
      repository: { full_name: "octo/other" },
      subject: { ...thread.subject, title: "Other issue", url: "/api/v3/repos/octo/other/issues/2" },
      url: "/api/v3/notifications/threads/t2",
    };
    mockFetch.mockImplementation((url: string) => {
      if (url.split("?")[0]! === "/api/v3/notifications") {
        return Promise.resolve(jsonResponse([thread, t2]));
      }
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "By repository" }));
    // Grouped view: the flat table is gone and two single-thread groups render
    // (each header carries a "(1)" count).
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.getAllByText("(1)")).toHaveLength(2);
    // The toggle reflects the active mode.
    expect(screen.getByRole("button", { name: "By repository" })).toHaveAttribute("aria-pressed", "true");
    // Thread links still resolve under their repo group.
    expect(screen.getByRole("link", { name: "Issue title" })).toHaveAttribute(
      "href",
      "/ui/repos/admin/repo/issues/1",
    );
    expect(screen.getByRole("link", { name: "Other issue" })).toHaveAttribute(
      "href",
      "/ui/repos/octo/other/issues/2",
    );
  });

  it("switches to all notifications", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());
    fireEvent.click(screen.getByText("All"));
    await waitFor(() => {
      expect(screen.getByText("All")).toBeInTheDocument();
      expect(screen.getByText("Issue title")).toBeInTheDocument();
    });
    expect(
      mockFetch.mock.calls.some(([u]) => {
        const s = String(u);
        return s.startsWith("/api/v3/notifications?") && s.includes("all=true");
      }),
    ).toBe(true);
  });

  it("marks a thread read", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("Mark read")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Mark read"));
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/api/v3/notifications/threads/t1",
        expect.objectContaining({ method: "PATCH" }),
      );
    });
  });

  it("opens subscription dialog", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("Subscription")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Subscription"));
    await waitFor(() => {
      expect(screen.getByText("Thread subscription")).toBeInTheDocument();
      expect(screen.getByText("Subscribed:")).toBeInTheDocument();
    });
  });

  it("marks every unread notification read and can mark one done", async () => {
    mockEndpoints();
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Mark all as read" }));
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/api/v3/notifications",
        expect.objectContaining({ method: "PUT" }),
      );
    });

    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/api/v3/notifications/threads/t1",
        expect.objectContaining({ method: "DELETE" }),
      );
    });
  });

  it("defaults to the by-repository grouping and persists a flat-list choice", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());
    // Grouped by default: no flat table, and the toggle reflects it.
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.getByRole("button", { name: "By repository" })).toHaveAttribute("aria-pressed", "true");

    fireEvent.click(screen.getByRole("button", { name: "List" }));
    expect(await screen.findByRole("table")).toBeInTheDocument();
    expect(localStorage.getItem("bleephub.notifications.group_by_repo")).toBe("flat");
  });

  it("bookmarks a thread into the Saved view and lists saved threads", async () => {
    mockEndpoints({ saved: [{ ...thread, saved: true }] });
    renderPage();
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());

    // The thread is already saved (per the saved view), so the toggle unsaves.
    fireEvent.click(screen.getByRole("button", { name: `Remove ${thread.subject.title} from saved` }));
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/ui-data/notifications/threads/t1/saved",
        expect.objectContaining({ method: "DELETE" }),
      );
    });

    // The Saved tab lists the /ui-data saved view.
    fireEvent.click(screen.getByRole("tab", { name: "Saved" }));
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());
    expect(
      mockFetch.mock.calls.some(([u]) => String(u).startsWith("/ui-data/notifications?view=saved")),
    ).toBe(true);
  });

  it("saves an unsaved inbox thread with PUT", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: `Save ${thread.subject.title}` }));
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/ui-data/notifications/threads/t1/saved",
        expect.objectContaining({ method: "PUT" }),
      );
    });
  });

  it("shows the Done view read-only (the server has no un-done endpoint)", async () => {
    mockEndpoints({ done: [{ ...thread, unread: false, saved: false }] });
    renderPage();
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("tab", { name: "Done" }));
    await waitFor(() => expect(screen.getByText(/kept here for reference/i)).toBeInTheDocument());
    expect(
      mockFetch.mock.calls.some(([u]) => String(u).startsWith("/ui-data/notifications?view=done")),
    ).toBe(true);
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());
    // No Done/Mark read/Subscription actions in the review surface; Saved
    // toggling stays available.
    expect(screen.queryByRole("button", { name: "Done" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Mark read" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Subscription" })).toBeNull();
    expect(screen.getByRole("button", { name: `Save ${thread.subject.title}` })).toBeInTheDocument();
  });

  it("marks a repository's notifications read from its group header", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("Issue title")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "By repository" }));
    fireEvent.click(await screen.findByRole("button", { name: "Mark all as read in admin/repo" }));
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/api/v3/repos/admin/repo/notifications",
        expect.objectContaining({ method: "PUT" }),
      );
    });
  });
});
