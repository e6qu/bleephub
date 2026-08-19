import { describe, it, expect } from "vitest";
import {
  parseQuery,
  filterAndSortItems,
  emptyFilters,
  type ListItemAccessors,
} from "../components/ListControls.js";

interface Row {
  title: string;
  body: string;
  labels: { name: string }[];
  author: string;
  assignees: string[];
  milestone: string | null;
  comments: number;
  created: string;
}

const acc: ListItemAccessors<Row> = {
  labels: (r) => r.labels,
  author: (r) => r.author,
  assignees: (r) => r.assignees,
  milestone: (r) => r.milestone,
  comments: (r) => r.comments,
  createdAt: (r) => r.created,
  updatedAt: (r) => r.created,
  text: (r) => `${r.title}\n${r.body}`,
};

const row = (over: Partial<Row>): Row => ({
  title: "a title",
  body: "a body",
  labels: [],
  author: "alice",
  assignees: [],
  milestone: null,
  comments: 0,
  created: "2026-01-01T00:00:00Z",
  ...over,
});

describe("parseQuery", () => {
  it("keeps free text instead of silently dropping it", () => {
    const { filters } = parseQuery("is:issue is:open crash on save");
    expect(filters.text).toBe("crash on save");
  });

  it("keeps unknown qualifiers as free text", () => {
    const { filters } = parseQuery("is:open weird:thing hello");
    expect(filters.text).toBe("weird:thing hello");
  });

  it("parses no:label, no:milestone and no:assignee", () => {
    const { filters } = parseQuery("is:open no:label no:milestone no:assignee");
    expect(filters.noLabel).toBe(true);
    expect(filters.noMilestone).toBe(true);
    expect(filters.noAssignee).toBe(true);
    expect(filters.text).toBeNull();
  });

  it("still parses the recognised qualifiers", () => {
    const { state, filters } = parseQuery('is:closed label:"help wanted" author:bob find me');
    expect(state).toBe("closed");
    expect(filters.label).toBe("help wanted");
    expect(filters.author).toBe("bob");
    expect(filters.text).toBe("find me");
  });
});

describe("filterAndSortItems free text + no: qualifiers", () => {
  it("filters by title/body substring, ANDing all terms", () => {
    const items = [
      row({ title: "crash on save", body: "" }),
      row({ title: "unrelated", body: "mentions crash only" }),
      row({ title: "save works", body: "no problems" }),
    ];
    const out = filterAndSortItems(items, { ...emptyFilters, text: "crash save" }, acc);
    expect(out.map((r) => r.title)).toEqual(["crash on save"]);
  });

  it("applies no:label / no:milestone / no:assignee", () => {
    const items = [
      row({ title: "bare" }),
      row({ title: "labeled", labels: [{ name: "bug" }] }),
      row({ title: "milestoned", milestone: "v1" }),
      row({ title: "assigned", assignees: ["bob"] }),
    ];
    expect(
      filterAndSortItems(items, { ...emptyFilters, noLabel: true }, acc).map((r) => r.title),
    ).toEqual(["bare", "milestoned", "assigned"]);
    expect(
      filterAndSortItems(items, { ...emptyFilters, noMilestone: true }, acc).map((r) => r.title),
    ).toEqual(["bare", "labeled", "assigned"]);
    expect(
      filterAndSortItems(items, { ...emptyFilters, noAssignee: true }, acc).map((r) => r.title),
    ).toEqual(["bare", "labeled", "milestoned"]);
  });

  it("skips the text filter when the caller provides no text accessor", () => {
    const { text: _drop, ...noText } = acc;
    const items = [row({ title: "crash" }), row({ title: "other" })];
    const out = filterAndSortItems(items, { ...emptyFilters, text: "crash" }, noText);
    expect(out).toHaveLength(2);
  });
});
