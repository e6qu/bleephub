import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup, screen } from "@testing-library/react";
import { TimelineEventRow } from "../components/TimelineEventRow.js";

afterEach(cleanup);

describe("TimelineEventRow", () => {
  it("renders a committed event with the message subject and a short-sha link", () => {
    render(
      <TimelineEventRow
        item={{
          event: "committed",
          sha: "abcdef1234567890abcdef1234567890abcdef12",
          message: "Fix the flux capacitor\n\nLonger body",
          html_url: "http://localhost/o/r/commit/abcdef1234567890abcdef1234567890abcdef12",
          author: { name: "Doc Brown", date: "2026-01-05T00:00:00Z" },
        }}
      />,
    );
    expect(screen.getByText(/added a commit/)).toBeInTheDocument();
    expect(screen.getByText(/Fix the flux capacitor/)).toBeInTheDocument();
    // Long body line is not shown, only the subject.
    expect(screen.queryByText(/Longer body/)).toBeNull();
    const link = screen.getByRole("link", { name: "abcdef1" });
    expect(link).toHaveAttribute(
      "href",
      "http://localhost/o/r/commit/abcdef1234567890abcdef1234567890abcdef12",
    );
    // Git author name shows even though there is no account actor.
    expect(screen.getByText("Doc Brown")).toBeInTheDocument();
  });

  it("renders head_ref_force_pushed with from/to short shas when both are present", () => {
    render(
      <TimelineEventRow
        item={{
          event: "head_ref_force_pushed",
          actor: { login: "alice", avatar_url: "" },
          created_at: "2026-01-06T00:00:00Z",
          before: "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
          after: "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        }}
      />,
    );
    expect(screen.getByText(/force-pushed the head branch/)).toBeInTheDocument();
    expect(screen.getByText("1111111")).toBeInTheDocument();
    expect(screen.getByText("2222222")).toBeInTheDocument();
    expect(screen.getByText("alice")).toBeInTheDocument();
  });

  it("falls back to commit_id for a force-push without before/after", () => {
    render(
      <TimelineEventRow
        item={{
          event: "head_ref_force_pushed",
          actor: { login: "alice", avatar_url: "" },
          commit_id: "3333333ccccccccccccccccccccccccccccccccc",
        }}
      />,
    );
    expect(screen.getByText(/force-pushed the head branch/)).toBeInTheDocument();
    expect(screen.getByText("3333333")).toBeInTheDocument();
  });

  it("renders head-ref deleted/restored, transferred and pinned/unpinned events", () => {
    const { unmount } = render(
      <TimelineEventRow item={{ event: "head_ref_deleted", actor: { login: "bob", avatar_url: "" } }} />,
    );
    expect(screen.getByText(/deleted the head branch/)).toBeInTheDocument();
    unmount();

    render(<TimelineEventRow item={{ event: "head_ref_restored", actor: { login: "bob", avatar_url: "" } }} />);
    expect(screen.getByText(/restored the head branch/)).toBeInTheDocument();
    cleanup();

    render(<TimelineEventRow item={{ event: "transferred", actor: { login: "bob", avatar_url: "" } }} />);
    expect(screen.getByText(/transferred this issue/)).toBeInTheDocument();
    cleanup();

    render(<TimelineEventRow item={{ event: "pinned", actor: { login: "bob", avatar_url: "" } }} />);
    expect(screen.getByText(/pinned this issue/)).toBeInTheDocument();
    cleanup();

    render(<TimelineEventRow item={{ event: "unpinned", actor: { login: "bob", avatar_url: "" } }} />);
    expect(screen.getByText(/unpinned this issue/)).toBeInTheDocument();
  });

  it("renders event dates as relative <time> elements", () => {
    render(
      <TimelineEventRow
        item={{ event: "closed", actor: { login: "alice", avatar_url: "" }, created_at: "2026-01-06T00:00:00Z" }}
      />,
    );
    const time = document.querySelector("time");
    expect(time).not.toBeNull();
    expect(time).toHaveAttribute("datetime", "2026-01-06T00:00:00Z");
  });
});
