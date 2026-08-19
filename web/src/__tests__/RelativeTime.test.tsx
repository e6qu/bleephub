import { describe, it, expect, afterEach, vi } from "vitest";
import { render, cleanup, screen } from "@testing-library/react";
import { formatRelative } from "../utils/relativeTime.js";
import { RelativeTime } from "../components/RelativeTime.js";

afterEach(cleanup);

describe("formatRelative", () => {
  const now = new Date("2026-08-19T12:00:00Z");
  const ago = (seconds: number) => new Date(now.getTime() - seconds * 1000).toISOString();

  it("covers each tier from seconds to the month boundary", () => {
    expect(formatRelative(ago(10), now)).toBe("just now");
    expect(formatRelative(ago(44), now)).toBe("just now");
    expect(formatRelative(ago(50), now)).toBe("1 minute ago");
    expect(formatRelative(ago(5 * 60), now)).toBe("5 minutes ago");
    expect(formatRelative(ago(2 * 3600), now)).toBe("2 hours ago");
    expect(formatRelative(ago(23 * 3600), now)).toBe("23 hours ago");
    expect(formatRelative(ago(3 * 86400), now)).toBe("3 days ago");
    expect(formatRelative(ago(6 * 86400), now)).toBe("6 days ago");
    expect(formatRelative(ago(10 * 86400), now)).toBe("1 week ago");
    expect(formatRelative(ago(21 * 86400), now)).toBe("3 weeks ago");
  });

  it("switches to an absolute date after ~a month, with the year only when it differs", () => {
    expect(formatRelative(ago(31 * 86400), now)).toBe("on Jul 19");
    expect(formatRelative("2025-06-03T00:00:00Z", now)).toBe("on Jun 3, 2025");
  });

  it("handles clock skew and bad input the way format.ts does", () => {
    expect(formatRelative(ago(-30), now)).toBe("just now");
    expect(formatRelative("not-a-date", now)).toBe("");
    expect(formatRelative("", now)).toBe("");
  });
});

describe("RelativeTime", () => {
  it("renders a <time> with machine-readable dateTime and a full-title hover", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-03-01T12:00:00Z"));
    try {
      const iso = "2026-03-01T10:00:00.000Z";
      render(<RelativeTime iso={iso} />);
      const time = screen.getByText("2 hours ago");
      expect(time.tagName).toBe("TIME");
      expect(time).toHaveAttribute("datetime", iso);
      expect(time.getAttribute("title")).toMatch(/\d{4}/);
    } finally {
      vi.useRealTimers();
    }
  });

  it("renders nothing for missing or unparsable input", () => {
    const { container } = render(<RelativeTime iso={undefined} />);
    expect(container).toBeEmptyDOMElement();
    const { container: bad } = render(<RelativeTime iso="garbage" />);
    expect(bad).toBeEmptyDOMElement();
  });
});
