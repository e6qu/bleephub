import { describe, expect, it } from "vitest";

// Every source file of the app as text, tests excluded.
const sources = import.meta.glob<string>(["../**/*.{ts,tsx}", "!../**/__tests__/**", "!../**/*.test.{ts,tsx}"], {
  query: "?raw",
  import: "default",
  eager: true,
});

describe("network exit point", () => {
  // Sign-out stops the page's traffic by aborting apiFetch's shared signal; a
  // bare fetch would slip past it and could still 401 and redirect mid-logout.
  it("sends every request through apiFetch", () => {
    expect(Object.keys(sources)).toContain("../api.ts");
    const offenders = Object.entries(sources)
      .filter(([path, text]) => path !== "../api.ts" && /(^|[^\w.])fetch\(/m.test(text))
      .map(([path]) => path);
    expect(offenders).toEqual([]);
  });
});
