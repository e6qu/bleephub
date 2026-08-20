/**
 * GitHub surfaces (discussion categories, reactions metadata) carry emoji as
 * `:shortcode:` strings; github.com renders the glyph. This maps the codes
 * those surfaces actually emit — the discussion-category defaults plus the
 * common picker set — and leaves unknown codes as-is rather than guessing.
 */
const SHORTCODES: Record<string, string> = {
  speech_balloon: "💬",
  bulb: "💡",
  question: "❓",
  raised_hands: "🙌",
  bar_chart: "📊",
  pray: "🙏",
  hash: "#️⃣",
  loudspeaker: "📢",
  mega: "📣",
  rocket: "🚀",
  tada: "🎉",
  heart: "❤️",
  eyes: "👀",
  thumbsup: "👍",
  "+1": "👍",
  thumbsdown: "👎",
  "-1": "👎",
  laughing: "😆",
  smile: "😄",
  confused: "😕",
  warning: "⚠️",
  sparkles: "✨",
  bug: "🐛",
  memo: "📝",
  books: "📚",
  wrench: "🔧",
  gear: "⚙️",
  lock: "🔒",
  bell: "🔔",
  calendar: "📅",
  checkered_flag: "🏁",
  trophy: "🏆",
  fire: "🔥",
  star: "⭐",
};

/** Replace every known `:shortcode:` in the string with its glyph. */
export function renderEmojiShortcodes(text: string | null | undefined): string {
  if (!text) return "";
  return text.replace(/:([a-z0-9_+-]+):/g, (match, code: string) => SHORTCODES[code] ?? match);
}
