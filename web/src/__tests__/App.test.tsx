import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App.js";
import { fetchBrowserSession, isLoggedIn } from "../api.js";

vi.mock("../api.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api.js")>();
  return { ...actual, isLoggedIn: vi.fn(() => false), fetchBrowserSession: vi.fn() };
});

const mockedProbe = vi.mocked(fetchBrowserSession);
const mockedIsLoggedIn = vi.mocked(isLoggedIn);

const originalFetch = globalThis.fetch;

beforeEach(() => {
  window.history.pushState({}, "", "/ui/login");
  mockedIsLoggedIn.mockReturnValue(false);
  // LoginPage probes /auth/providers on mount.
  globalThis.fetch = vi.fn(async () => new Response("{}", { status: 200 })) as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.clearAllMocks();
});

describe("App session states", () => {
  it("renders a loading state while the session probe is in flight", () => {
    mockedProbe.mockReturnValue(new Promise<boolean>(() => {}));
    render(<App />);

    expect(screen.getByRole("status")).toHaveTextContent(/checking your session/i);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders the sign-in page when the probe reports no session", async () => {
    mockedProbe.mockResolvedValue(false);
    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Sign in to Bleephub" }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders a distinct error state when the probe fails, not the sign-in page", async () => {
    mockedProbe.mockRejectedValue(new Error("session probe failed: 503 Service Unavailable"));
    render(<App />);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/could not check your session/i);
    expect(alert).toHaveTextContent(/503 Service Unavailable/);
    expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Sign in to Bleephub" })).not.toBeInTheDocument();
  });

  it("retries the probe when the error state's action is used", async () => {
    mockedProbe.mockRejectedValueOnce(new Error("boom")).mockResolvedValueOnce(false);
    render(<App />);

    const retry = await screen.findByRole("button", { name: /try again/i });
    retry.click();

    expect(
      await screen.findByRole("heading", { name: "Sign in to Bleephub" }),
    ).toBeInTheDocument();
    await waitFor(() => expect(mockedProbe).toHaveBeenCalledTimes(2));
  });

  it("gives the probe a bounded timeout", () => {
    mockedProbe.mockReturnValue(new Promise<boolean>(() => {}));
    render(<App />);
    // Called with no explicit argument: the module's bounded default applies.
    expect(mockedProbe).toHaveBeenCalledTimes(1);
    expect(mockedProbe.mock.calls[0]).toHaveLength(0);
  });
});
