import { describe, it, expect } from "vitest";
import { lastPageFromLink, totalUpperBoundFromLink } from "../utils/paging.js";

const LINK =
  '<http://x/api/v3/repos/o/r/issues?per_page=30&page=2>; rel="next", ' +
  '<http://x/api/v3/repos/o/r/issues?per_page=30&page=7>; rel="last"';

describe("lastPageFromLink", () => {
  it("extracts the rel=last page number", () => {
    expect(lastPageFromLink(LINK)).toBe(7);
  });

  it("returns null without a header or a rel=last", () => {
    expect(lastPageFromLink(null)).toBeNull();
    expect(lastPageFromLink('<http://x/things?page=3>; rel="next"')).toBeNull();
    expect(lastPageFromLink("")).toBeNull();
  });
});

describe("totalUpperBoundFromLink", () => {
  it("multiplies the last page by per_page for a count upper bound", () => {
    expect(totalUpperBoundFromLink(LINK, 30)).toBe(210);
  });

  it("returns null for a single page or a bogus per_page", () => {
    expect(totalUpperBoundFromLink(null, 30)).toBeNull();
    expect(totalUpperBoundFromLink(LINK, 0)).toBeNull();
  });
});
