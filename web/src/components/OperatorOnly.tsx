import { StatCard } from "./ui.js";

/**
 * Stand-in for figures the server serves only to site admins.
 *
 * A refusal on authorization grounds is an answer about who is asking, not a
 * fault, so this deliberately carries no alert semantics: the cards keep their
 * shape and the reason is stated plainly.
 */
export function OperatorOnlyStats({ titles }: { titles: string[] }) {
  return (
    <>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {titles.map((title) => (
          <StatCard key={title} title={title} value="—" />
        ))}
      </div>
      <p
        role="note"
        className="mt-2"
        style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}
      >
        These figures are instance diagnostics and require site admin.
      </p>
    </>
  );
}
