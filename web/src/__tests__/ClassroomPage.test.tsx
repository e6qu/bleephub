import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { ClassroomPage } from "../pages/ClassroomPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => { cleanup(); mockFetch.mockReset(); });

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}><Routes>
    <Route path="/ui/classrooms" element={<ClassroomPage />} />
    <Route path="/ui/classrooms/:classroomId" element={<ClassroomPage />} />
    <Route path="/ui/classrooms/accept/:inviteCode" element={<ClassroomPage />} />
    <Route path="/ui/repos/:owner/:repo" element={<div>Repository opened</div>} />
  </Routes></MemoryRouter></QueryClientProvider>);
}

const classroom = {
  id: 41,
  name: "Systems Programming",
  archived: false,
  url: "/classrooms/41-systems-programming",
  organization: { id: 7, login: "octo-school", name: "Octo School", avatar_url: "" },
  roster: [{ id: 8, login: "mona", avatar_url: "", roster_identifier: "mona@example.edu" }],
  assignments: [{ id: 51, title: "Processes", type: "individual", slug: "processes", invite_link: "http://x/a/code", invitations_enabled: true, public_repo: false, accepted: 3, submitted: 2, passing: 1, deadline: null, autograding_tests: [{ name: "Tests", command: "go test ./...", points: 10 }] }],
};

describe("ClassroomPage", () => {
  it("accepts a typed classroom name and creates it in the selected organization", async () => {
    const requests: Array<{ url: string; method: string; body?: unknown }> = [];
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input.toString();
      const method = init?.method ?? "GET";
      requests.push({
        url,
        method,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      if (method === "POST" && url === "/classroom-data/classrooms") {
        return Promise.resolve(jsonResponse({ ...classroom, id: 52, name: "Operating Systems" }, 201));
      }
      return Promise.resolve(jsonResponse({
        classrooms: [],
        organizations: [classroom.organization],
        can_create_organization: true,
      }));
    });

    renderAt("/ui/classrooms");
    fireEvent.click(await screen.findByRole("button", { name: "New classroom" }));
    const name = screen.getByLabelText("Classroom name") as HTMLInputElement;
    fireEvent.change(name, { target: { value: "Operating Systems" } });
    expect(name.value).toBe("Operating Systems");
    fireEvent.click(screen.getByRole("button", { name: "Create classroom" }));

    await waitFor(() => {
      expect(requests).toContainEqual({
        url: "/classroom-data/classrooms",
        method: "POST",
        body: { name: "Operating Systems", organization: "octo-school" },
      });
    });
  });

  it("lets a site administrator create the required organization without leaving Classroom", async () => {
    let organizationCreated = false;
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input.toString();
      const method = init?.method ?? "GET";
      if (url === "/api/v3/user") {
        return Promise.resolve(jsonResponse({ id: 1, login: "admin", site_admin: true }));
      }
      if (url === "/api/v3/admin/organizations" && method === "POST") {
        organizationCreated = true;
        return Promise.resolve(jsonResponse({ ...classroom.organization, login: "new-school" }, 201));
      }
      return Promise.resolve(jsonResponse({
        classrooms: [],
        organizations: organizationCreated
          ? [{ ...classroom.organization, login: "new-school" }]
          : [],
        can_create_organization: true,
      }));
    });

    renderAt("/ui/classrooms");
    fireEvent.click(await screen.findByRole("button", { name: "New classroom" }));
    fireEvent.change(screen.getByLabelText("Organization login"), {
      target: { value: "new-school" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create organization" }));

    expect(await screen.findByLabelText("Classroom name")).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /new-school/ })).toBeInTheDocument();
  });

  it("renders the retained Classroom dashboard and real coursework counters", async () => {
    mockFetch.mockResolvedValue(jsonResponse({ classrooms: [classroom], organizations: [classroom.organization], can_create_organization: true }));
    renderAt("/ui/classrooms");
    expect(await screen.findByText("GitHub Classroom, kept alive.")).toBeInTheDocument();
    expect(screen.getByText("Systems Programming")).toBeInTheDocument();
    expect(screen.getByText((_text, element) => element?.tagName === "SPAN" && element.textContent === "1 assignments")).toBeInTheDocument();
    expect(screen.getByText((_text, element) => element?.tagName === "SPAN" && element.textContent === "1 students")).toBeInTheDocument();
  });

  it("renders assignment organization, roster, autograding, and status detail", async () => {
    mockFetch.mockResolvedValue(jsonResponse({ classrooms: [classroom], organizations: [classroom.organization], can_create_organization: true }));
    renderAt("/ui/classrooms/41");
    expect(await screen.findByRole("heading", { name: "Systems Programming" })).toBeInTheDocument();
    expect(screen.getByText("Processes")).toBeInTheDocument();
    expect(screen.getByText("3 accepted")).toBeInTheDocument();
    expect(screen.getByText("1 passing")).toBeInTheDocument();
    expect(screen.getByText("10 autograding points")).toBeInTheDocument();
  });

  it("creates an assignment from a readable repository selection", async () => {
    const requests: Array<{ url: string; method: string; body?: unknown }> = [];
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input.toString();
      const method = init?.method ?? "GET";
      requests.push({
        url,
        method,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      if (url === "/api/v3/user/repos?per_page=100") {
        return Promise.resolve(jsonResponse([{
          id: 99,
          full_name: "octo-school/processes-starter",
          name: "processes-starter",
          owner: { login: "octo-school", type: "Organization" },
        }]));
      }
      if (url === "/classroom-data/classrooms/41/assignments" && method === "POST") {
        return Promise.resolve(jsonResponse({
          ...classroom.assignments[0],
          id: 52,
          title: "Threads",
        }, 201));
      }
      return Promise.resolve(jsonResponse({
        classrooms: [classroom],
        organizations: [classroom.organization],
        can_create_organization: true,
      }));
    });

    renderAt("/ui/classrooms/41");
    fireEvent.click(await screen.findByRole("button", { name: "New assignment" }));
    fireEvent.change(screen.getByLabelText("Assignment title"), { target: { value: "Threads" } });
    await screen.findByRole("option", { name: "octo-school/processes-starter" });
    fireEvent.change(screen.getByLabelText("Starter repository"), {
      target: { value: "octo-school/processes-starter" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create assignment" }));

    await waitFor(() => expect(requests.some((request) =>
      request.url === "/classroom-data/classrooms/41/assignments"
      && request.method === "POST"
      && (request.body as { starter_code_repository?: string }).starter_code_repository
        === "octo-school/processes-starter",
    )).toBe(true));
  });

  it("renames classrooms and edits or deletes assignments through their management dialogs", async () => {
    const requests: Array<{ url: string; method: string; body?: unknown }> = [];
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input.toString();
      const method = init?.method ?? "GET";
      requests.push({
        url,
        method,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      if (method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
      if (method === "PATCH" && url.includes("/assignments/")) {
        return Promise.resolve(jsonResponse({ ...classroom.assignments[0], ...JSON.parse(init!.body as string) }));
      }
      if (method === "PATCH") {
        return Promise.resolve(jsonResponse({ ...classroom, ...JSON.parse(init!.body as string) }));
      }
      return Promise.resolve(jsonResponse({
        classrooms: [classroom],
        organizations: [classroom.organization],
        can_create_organization: true,
      }));
    });

    renderAt("/ui/classrooms/41");
    fireEvent.click(await screen.findByRole("button", { name: "Classroom settings" }));
    fireEvent.change(screen.getByLabelText("Classroom name"), { target: { value: "Advanced Systems" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(requests).toContainEqual({
      url: "/classroom-data/classrooms/41",
      method: "PATCH",
      body: { name: "Advanced Systems" },
    }));

    fireEvent.click(await screen.findByRole("button", { name: "Edit assignment" }));
    fireEvent.change(screen.getByLabelText("Assignment title"), { target: { value: "Processes II" } });
    fireEvent.click(screen.getByRole("button", { name: "Save assignment" }));
    await waitFor(() => expect(requests.some((request) =>
      request.url === "/classroom-data/assignments/51"
      && request.method === "PATCH"
      && (request.body as { title?: string }).title === "Processes II",
    )).toBe(true));

    fireEvent.click(await screen.findByRole("button", { name: "Edit assignment" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete assignment" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete permanently" }));
    await waitFor(() => expect(requests).toContainEqual({
      url: "/classroom-data/assignments/51",
      method: "DELETE",
      body: {},
    }));
  });

  it("accepts an invite and hands off to the generated repository", async () => {
    mockFetch.mockImplementation((_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "POST") return Promise.resolve(jsonResponse({ id: 1, repository: { full_name: "octo-school/processes-mona", html_url: "/octo-school/processes-mona" } }, 201));
      return Promise.resolve(jsonResponse({ ...classroom.assignments[0], starter_code_repository: { full_name: "octo-school/processes-starter" } }));
    });
    renderAt("/ui/classrooms/accept/code");
    expect(await screen.findByText("Processes")).toBeInTheDocument();
    screen.getByRole("button", { name: "Accept this assignment" }).click();
    await waitFor(() => expect(screen.getByText("Repository opened")).toBeInTheDocument());
  });
});
