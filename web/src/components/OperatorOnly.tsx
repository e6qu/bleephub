import { StatCard } from "./ui.js";

// Placeholder for site-admin-only figures. No alert semantics: an authz refusal
// is an answer about who's asking, not a fault.
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
