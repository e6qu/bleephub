import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router";
import { GoToFile } from "../components/GoToFile.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="loc">{loc.pathname}</div>;
}

const tree = {
  sha: "abc",
  truncated: false,
  tree: [
    { path: "README.md", type: "blob", mode: "100644", sha: "1" },
    { path: "src/index.ts", type: "blob", mode: "100644", sha: "2" },
    { path: "src", type: "tree", mode: "040000", sha: "3" },
    { path: "src/api/client.ts", type: "blob", mode: "100644", sha: "4" },
  ],
};

function renderFinder(onClose = vi.fn()) {
  mockFetch.mockImplementation((url: RequestInfo | URL) => {
    if (url.toString().includes("/git/trees/main?recursive=1")) return Promise.resolve(jsonResponse(tree));
    return Promise.resolve(jsonResponse({}));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/ui/o/r"]}>
        <GoToFile owner="o" repo="r" gitRef="main" onClose={onClose} />
        <Routes>
          <Route path="*" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { onClose };
}

describe("GoToFile", () => {
  it("lists blobs (not trees) and fuzzy-filters by path", async () => {
    renderFinder();
    // The dialog + combobox render, and blobs are listed (the tree entry is excluded).
    expect(await screen.findByRole("option", { name: "README.md" })).toBeInTheDocument();
    expect(await screen.findByRole("option", { name: "src/index.ts" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "src" })).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "client" } });
    await waitFor(() => {
      expect(screen.getByRole("option", { name: "src/api/client.ts" })).toBeInTheDocument();
      expect(screen.queryByRole("option", { name: "README.md" })).not.toBeInTheDocument();
    });
  });

  it("navigates to the chosen file's blob route on Enter", async () => {
    renderFinder();
    await screen.findByRole("option", { name: "README.md" });
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "index" } });
    await screen.findByRole("option", { name: "src/index.ts" });
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
    await waitFor(() => {
      expect(screen.getByTestId("loc").textContent).toBe("/ui/o/r/blob/main/src/index.ts");
    });
  });

  it("closes on Escape", async () => {
    const { onClose } = renderFinder();
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
    expect(onClose).toHaveBeenCalled();
  });
});
