// Language dot colors mirroring github/linguist's languages.yml; unlisted → neutral gray.
export const LANGUAGE_COLORS: Record<string, string> = {
  Go: "#00ADD8",
  TypeScript: "#3178c6",
  JavaScript: "#f1e05a",
  Python: "#3572A5",
  Rust: "#dea584",
  Java: "#b07219",
  C: "#555555",
  "C++": "#f34b7d",
  "C#": "#178600",
  Ruby: "#701516",
  PHP: "#4F5D95",
  Shell: "#89e051",
  HTML: "#e34c26",
  CSS: "#663399",
  Swift: "#F05138",
  Kotlin: "#A97BFF",
  Dart: "#00B4AB",
  Scala: "#c22d40",
  Elixir: "#6e4a7e",
  Haskell: "#5e5086",
  Lua: "#000080",
  Vue: "#41b883",
  "Objective-C": "#438eff",
  Dockerfile: "#384d54",
  Makefile: "#427819",
  Markdown: "#083fa1",
};

export function languageColor(language: string): string {
  return LANGUAGE_COLORS[language] ?? "#8b949e";
}
