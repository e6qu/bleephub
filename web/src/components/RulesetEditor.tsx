import { useEffect, useMemo, useRef, useState } from "react";
import type { GithubRuleset, GithubRulesetTarget } from "../types.js";
import { Button, FormLabel } from "./ui.js";

// The fragment of a ruleset payload this editor owns: everything except the
// name/target/enforcement, which the host form manages. `conditions` is omitted
// entirely when no ref-name targeting is set (GitHub then applies to all refs).
export interface RulesetRuleConfig {
  rules: Array<{ type: string; parameters?: Record<string, unknown> }>;
  conditions?: { ref_name: { include: string[]; exclude: string[] } };
  bypass_actors: Array<{ actor_id: number; actor_type: string; bypass_mode: string }>;
}

// Simple no-parameter rules the evaluator enforces directly.
const SIMPLE_RULES: Array<{ type: string; label: string }> = [
  { type: "creation", label: "Restrict creations" },
  { type: "update", label: "Restrict updates" },
  { type: "deletion", label: "Restrict deletions" },
  { type: "non_fast_forward", label: "Block force pushes" },
  { type: "required_linear_history", label: "Require linear history" },
  { type: "required_signatures", label: "Require signed commits" },
];

// GitHub's actor_type + bypass_mode enums for the bypass list.
const ACTOR_TYPES = ["User", "Team", "Integration", "OrganizationAdmin", "RepositoryRole", "DeployKey"];
const BYPASS_MODES = ["always", "pull_request"];

// GitHub's fixed actor_id values for role-based bypass actors: RepositoryRole
// uses the documented role ids (2 = maintain, 4 = write, 5 = admin) and
// OrganizationAdmin always uses actor_id 1. DeployKey carries no meaningful id.
const REPOSITORY_ROLES: Array<{ id: number; label: string }> = [
  { id: 5, label: "Repository admin" },
  { id: 2, label: "Maintain" },
  { id: 4, label: "Write" },
];
const ORGANIZATION_ADMIN_ACTOR_ID = 1;

/** Minimal team shape the bypass picker needs (GithubOrgTeam satisfies it). */
export interface RulesetTeamOption {
  id: number;
  name: string;
  slug?: string;
}
const PATTERN_OPERATORS = ["starts_with", "ends_with", "contains", "regex"];

// splitPatterns turns a newline/comma-separated textarea into a trimmed list.
function splitPatterns(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

// GitHub's repository-rule-pull-request requires all five of these fields.
interface PullRequestParams {
  required_approving_review_count: number;
  dismiss_stale_reviews_on_push: boolean;
  require_code_owner_review: boolean;
  require_last_push_approval: boolean;
  required_review_thread_resolution: boolean;
}

interface NamePatternParams {
  operator: string;
  pattern: string;
  negate: boolean;
}

function ruleByType(ruleset: GithubRuleset | null | undefined, type: string) {
  return ruleset?.rules?.find((r) => r.type === type);
}

// RulesetEditor renders GitHub's ruleset targeting + rule + bypass controls and
// reports the assembled config to the host on every change. It is deliberately
// self-contained so the org modal and the repo settings tab share one authoring
// surface (no duplication) without either becoming entry-resident.
export function RulesetEditor({
  target,
  initial,
  onChange,
  teams,
}: {
  target: GithubRulesetTarget;
  initial?: GithubRuleset | null;
  onChange: (config: RulesetRuleConfig) => void;
  /**
   * Optional org teams for the Team bypass-actor picker. When absent (e.g. a
   * user-owned repo, or a host that has not wired it) the Team type falls back
   * to a raw numeric id input — a purely additive prop, existing hosts keep
   * their behavior.
   */
  teams?: RulesetTeamOption[];
}) {
  const namePatternType = target === "tag" ? "tag_name_pattern" : "branch_name_pattern";
  const showRefConditions = target === "branch" || target === "tag";

  // ── targeting (conditions.ref_name) ──────────────────────────────────────
  const initialInclude = initial?.conditions?.ref_name?.include ?? [];
  const initialExclude = initial?.conditions?.ref_name?.exclude ?? [];
  const [includeDefault, setIncludeDefault] = useState(initialInclude.includes("~DEFAULT_BRANCH"));
  const [includeAll, setIncludeAll] = useState(initialInclude.includes("~ALL"));
  const [includePatterns, setIncludePatterns] = useState(
    initialInclude.filter((p) => p !== "~DEFAULT_BRANCH" && p !== "~ALL").join("\n"),
  );
  const [excludePatterns, setExcludePatterns] = useState(initialExclude.join("\n"));

  // ── simple rules ─────────────────────────────────────────────────────────
  const [enabled, setEnabled] = useState<Set<string>>(
    () => new Set(SIMPLE_RULES.map((r) => r.type).filter((t) => ruleByType(initial, t))),
  );

  // ── pull_request ─────────────────────────────────────────────────────────
  const prInit = ruleByType(initial, "pull_request")?.parameters as Record<string, unknown> | undefined;
  const [prEnabled, setPrEnabled] = useState(Boolean(ruleByType(initial, "pull_request")));
  const [pr, setPr] = useState<PullRequestParams>({
    required_approving_review_count: Number(prInit?.["required_approving_review_count"] ?? 1),
    dismiss_stale_reviews_on_push: Boolean(prInit?.["dismiss_stale_reviews_on_push"]),
    require_code_owner_review: Boolean(prInit?.["require_code_owner_review"]),
    require_last_push_approval: Boolean(prInit?.["require_last_push_approval"]),
    required_review_thread_resolution: Boolean(prInit?.["required_review_thread_resolution"]),
  });

  // ── required_status_checks ───────────────────────────────────────────────
  const scInit = ruleByType(initial, "required_status_checks")?.parameters as Record<string, unknown> | undefined;
  const [scEnabled, setScEnabled] = useState(Boolean(ruleByType(initial, "required_status_checks")));
  const [scStrict, setScStrict] = useState(Boolean(scInit?.["strict_required_status_checks_policy"]));
  const [contexts, setContexts] = useState<string>(() => {
    const raw = (scInit?.["required_status_checks"] as Array<{ context?: string }> | undefined) ?? [];
    return raw.map((c) => (typeof c === "string" ? c : c.context ?? "")).filter(Boolean).join("\n");
  });

  // ── name pattern ─────────────────────────────────────────────────────────
  const npRule = ruleByType(initial, namePatternType);
  const npInit = npRule?.parameters as Record<string, unknown> | undefined;
  const [npEnabled, setNpEnabled] = useState(Boolean(npRule));
  const [np, setNp] = useState<NamePatternParams>({
    operator: String(npInit?.["operator"] ?? "starts_with"),
    pattern: String(npInit?.["pattern"] ?? ""),
    negate: Boolean(npInit?.["negate"]),
  });

  // ── bypass actors ────────────────────────────────────────────────────────
  const [bypass, setBypass] = useState<Array<{ actor_id: number; actor_type: string; bypass_mode: string }>>(
    () => (initial?.bypass_actors ?? []).map((a) => ({ actor_id: a.actor_id, actor_type: a.actor_type, bypass_mode: a.bypass_mode })),
  );

  const config = useMemo<RulesetRuleConfig>(() => {
    const rules: RulesetRuleConfig["rules"] = [];
    for (const r of SIMPLE_RULES) if (enabled.has(r.type)) rules.push({ type: r.type });
    if (prEnabled) {
      rules.push({
        type: "pull_request",
        parameters: {
          required_approving_review_count: pr.required_approving_review_count,
          dismiss_stale_reviews_on_push: pr.dismiss_stale_reviews_on_push,
          require_code_owner_review: pr.require_code_owner_review,
          require_last_push_approval: pr.require_last_push_approval,
          required_review_thread_resolution: pr.required_review_thread_resolution,
        },
      });
    }
    if (scEnabled) {
      rules.push({
        type: "required_status_checks",
        parameters: {
          required_status_checks: splitPatterns(contexts).map((context) => ({ context })),
          strict_required_status_checks_policy: scStrict,
        },
      });
    }
    if (npEnabled && np.pattern.trim()) {
      rules.push({
        type: namePatternType,
        parameters: { operator: np.operator, pattern: np.pattern.trim(), negate: np.negate },
      });
    }

    const include = [
      ...(includeAll ? ["~ALL"] : includeDefault ? ["~DEFAULT_BRANCH"] : []),
      ...splitPatterns(includePatterns),
    ];
    const exclude = splitPatterns(excludePatterns);
    const hasConditions = showRefConditions && (include.length > 0 || exclude.length > 0);
    return {
      rules,
      bypass_actors: bypass,
      ...(hasConditions ? { conditions: { ref_name: { include, exclude } } } : {}),
    };
  }, [
    enabled, prEnabled, pr, scEnabled, scStrict, contexts, npEnabled, np, namePatternType,
    includeAll, includeDefault, includePatterns, excludePatterns, showRefConditions, bypass,
  ]);

  // Report upward without making the parent re-render mid-render.
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  useEffect(() => {
    onChangeRef.current(config);
  }, [config]);

  const toggle = (type: string) =>
    setEnabled((prev) => {
      const next = new Set(prev);
      if (next.has(type)) next.delete(type);
      else next.add(type);
      return next;
    });

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {showRefConditions && (
        <fieldset style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", padding: "0.75rem" }}>
          <legend style={{ fontWeight: 600, fontSize: "0.85rem", padding: "0 0.3rem" }}>Target {target === "tag" ? "tags" : "branches"}</legend>
          <label style={condCheck}>
            <input type="checkbox" checked={includeDefault} disabled={includeAll} onChange={(e) => setIncludeDefault(e.target.checked)} />
            Include default branch
          </label>
          <label style={condCheck}>
            <input type="checkbox" checked={includeAll} onChange={(e) => setIncludeAll(e.target.checked)} />
            Include all {target === "tag" ? "tags" : "branches"}
          </label>
          <label style={{ display: "block", marginTop: "0.5rem" }}>
            <FormLabel>Include by pattern (one per line)</FormLabel>
            <textarea aria-label="Include ref patterns" value={includePatterns} onChange={(e) => setIncludePatterns(e.target.value)} rows={2} style={taStyle} placeholder={"refs/heads/release/*"} />
          </label>
          <label style={{ display: "block" }}>
            <FormLabel>Exclude by pattern (one per line)</FormLabel>
            <textarea aria-label="Exclude ref patterns" value={excludePatterns} onChange={(e) => setExcludePatterns(e.target.value)} rows={2} style={taStyle} />
          </label>
        </fieldset>
      )}

      <fieldset style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", padding: "0.75rem" }}>
        <legend style={{ fontWeight: 600, fontSize: "0.85rem", padding: "0 0.3rem" }}>Rules</legend>
        <div style={{ display: "flex", flexDirection: "column", gap: "0.35rem" }}>
          {SIMPLE_RULES.map((r) => (
            <label key={r.type} style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
              <input type="checkbox" checked={enabled.has(r.type)} onChange={() => toggle(r.type)} aria-label={r.type} />
              {r.label} <span style={mutedType}>{r.type}</span>
            </label>
          ))}

          <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
            <input type="checkbox" checked={prEnabled} onChange={(e) => setPrEnabled(e.target.checked)} aria-label="pull_request" />
            Require a pull request before merging <span style={mutedType}>pull_request</span>
          </label>
          {prEnabled && (
            <div style={subForm}>
              <label style={inlineField}>
                <span>Required approvals</span>
                <input type="number" min={0} max={10} aria-label="Required approving review count" value={pr.required_approving_review_count}
                  onChange={(e) => setPr({ ...pr, required_approving_review_count: Number(e.target.value) })} style={{ ...numStyle }} />
              </label>
              <label style={inlineCheck}>
                <input type="checkbox" checked={pr.dismiss_stale_reviews_on_push} onChange={(e) => setPr({ ...pr, dismiss_stale_reviews_on_push: e.target.checked })} />
                Dismiss stale reviews on push
              </label>
              <label style={inlineCheck}>
                <input type="checkbox" checked={pr.require_code_owner_review} onChange={(e) => setPr({ ...pr, require_code_owner_review: e.target.checked })} />
                Require review from Code Owners
              </label>
              <label style={inlineCheck}>
                <input type="checkbox" checked={pr.require_last_push_approval} onChange={(e) => setPr({ ...pr, require_last_push_approval: e.target.checked })} />
                Require approval of the most recent push
              </label>
              <label style={inlineCheck}>
                <input type="checkbox" checked={pr.required_review_thread_resolution} onChange={(e) => setPr({ ...pr, required_review_thread_resolution: e.target.checked })} />
                Require conversation resolution before merging
              </label>
            </div>
          )}

          <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
            <input type="checkbox" checked={scEnabled} onChange={(e) => setScEnabled(e.target.checked)} aria-label="required_status_checks" />
            Require status checks to pass <span style={mutedType}>required_status_checks</span>
          </label>
          {scEnabled && (
            <div style={subForm}>
              <label style={{ display: "block", flexBasis: "100%" }}>
                <FormLabel>Required checks (one context per line)</FormLabel>
                <textarea aria-label="Required status check contexts" value={contexts} onChange={(e) => setContexts(e.target.value)} rows={2} style={taStyle} placeholder={"build\nlint"} />
              </label>
              <label style={inlineCheck}>
                <input type="checkbox" checked={scStrict} onChange={(e) => setScStrict(e.target.checked)} />
                Require branches to be up to date before merging
              </label>
            </div>
          )}

          {showRefConditions && (
            <>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={npEnabled} onChange={(e) => setNpEnabled(e.target.checked)} aria-label={namePatternType} />
                Restrict {target === "tag" ? "tag" : "branch"} names <span style={mutedType}>{namePatternType}</span>
              </label>
              {npEnabled && (
                <div style={subForm}>
                  <label style={inlineField}>
                    <span>Operator</span>
                    <select aria-label="Name pattern operator" value={np.operator} onChange={(e) => setNp({ ...np, operator: e.target.value })} style={selStyle}>
                      {PATTERN_OPERATORS.map((op) => <option key={op} value={op}>{op}</option>)}
                    </select>
                  </label>
                  <label style={inlineField}>
                    <span>Pattern</span>
                    <input aria-label="Name pattern" value={np.pattern} onChange={(e) => setNp({ ...np, pattern: e.target.value })} style={{ ...numStyle, width: "12rem" }} />
                  </label>
                  <label style={inlineCheck}>
                    <input type="checkbox" checked={np.negate} onChange={(e) => setNp({ ...np, negate: e.target.checked })} />
                    Negate (names must NOT match)
                  </label>
                </div>
              )}
            </>
          )}
        </div>
      </fieldset>

      <fieldset style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", padding: "0.75rem" }}>
        <legend style={{ fontWeight: 600, fontSize: "0.85rem", padding: "0 0.3rem" }}>Bypass list</legend>
        {bypass.length === 0 && <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: "0 0 0.5rem" }}>No actors can bypass this ruleset.</p>}
        {bypass.map((actor, i) => (
          <div key={i} style={{ display: "flex", flexWrap: "wrap", gap: "0.5rem", alignItems: "end", marginBottom: "0.5rem" }}>
            {actor.actor_type === "RepositoryRole" ? (
              <label style={inlineField}>
                <span>Role</span>
                <select aria-label={`Bypass actor ${i + 1} role`} value={actor.actor_id}
                  onChange={(e) => setBypass(bypass.map((a, j) => (j === i ? { ...a, actor_id: Number(e.target.value) } : a)))} style={selStyle}>
                  {REPOSITORY_ROLES.map((r) => <option key={r.id} value={r.id}>{r.label}</option>)}
                </select>
              </label>
            ) : actor.actor_type === "Team" && teams && teams.length > 0 ? (
              <label style={inlineField}>
                <span>Team</span>
                <select aria-label={`Bypass actor ${i + 1} team`} value={actor.actor_id}
                  onChange={(e) => setBypass(bypass.map((a, j) => (j === i ? { ...a, actor_id: Number(e.target.value) } : a)))} style={selStyle}>
                  <option value={0}>Select team…</option>
                  {teams.map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
                </select>
              </label>
            ) : actor.actor_type === "OrganizationAdmin" ? (
              <span style={{ ...inlineField, justifyContent: "end", color: "var(--color-fg-muted)" }}>
                <span>Actor ID</span>
                <span style={{ fontSize: "0.85rem", padding: "0.3rem 0" }}>{ORGANIZATION_ADMIN_ACTOR_ID} (fixed)</span>
              </span>
            ) : actor.actor_type === "DeployKey" ? null : (
              <label style={inlineField}>
                <span>Actor ID</span>
                <input type="number" aria-label={`Bypass actor ${i + 1} id`} value={actor.actor_id}
                  onChange={(e) => setBypass(bypass.map((a, j) => (j === i ? { ...a, actor_id: Number(e.target.value) } : a)))} style={numStyle} />
              </label>
            )}
            <label style={inlineField}>
              <span>Type</span>
              <select aria-label={`Bypass actor ${i + 1} type`} value={actor.actor_type}
                onChange={(e) => {
                  const nextType = e.target.value;
                  // Reset the id to the type's sensible default so a stale id
                  // from the previous type never leaks into the payload.
                  const nextId =
                    nextType === "RepositoryRole" ? REPOSITORY_ROLES[0]!.id
                    : nextType === "OrganizationAdmin" ? ORGANIZATION_ADMIN_ACTOR_ID
                    : 0;
                  setBypass(bypass.map((a, j) => (j === i ? { ...a, actor_type: nextType, actor_id: nextId } : a)));
                }} style={selStyle}>
                {ACTOR_TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
              </select>
            </label>
            <label style={inlineField}>
              <span>Mode</span>
              <select aria-label={`Bypass actor ${i + 1} mode`} value={actor.bypass_mode}
                onChange={(e) => setBypass(bypass.map((a, j) => (j === i ? { ...a, bypass_mode: e.target.value } : a)))} style={selStyle}>
                {BYPASS_MODES.map((m) => <option key={m} value={m}>{m}</option>)}
              </select>
            </label>
            <Button type="button" size="sm" variant="ghost" onClick={() => setBypass(bypass.filter((_, j) => j !== i))}>Remove</Button>
          </div>
        ))}
        <Button type="button" size="sm" variant="secondary" onClick={() => setBypass([...bypass, { actor_id: 0, actor_type: "User", bypass_mode: "always" }])}>
          Add bypass actor
        </Button>
      </fieldset>
    </div>
  );
}

const taStyle = { width: "100%", fontSize: "0.8rem", fontFamily: "var(--font-mono, monospace)", borderRadius: "var(--radius-md)", border: "1px solid var(--color-border)", background: "var(--color-surface)", color: "var(--color-fg)", padding: "0.4rem 0.5rem", resize: "vertical" as const };
const numStyle = { width: "5rem", fontSize: "0.8rem", borderRadius: "var(--radius-md)", border: "1px solid var(--color-border)", background: "var(--color-surface)", color: "var(--color-fg)", padding: "0.3rem 0.5rem" };
const selStyle = { fontSize: "0.8rem", borderRadius: "var(--radius-md)", border: "1px solid var(--color-border)", background: "var(--color-surface)", color: "var(--color-fg)", padding: "0.3rem 0.5rem" };
const subForm = { display: "flex", flexWrap: "wrap" as const, gap: "0.6rem", alignItems: "end", padding: "0.4rem 0 0.4rem 1.6rem" };
const inlineField = { display: "flex", flexDirection: "column" as const, gap: "0.2rem", fontSize: "0.8rem" };
const inlineCheck = { display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.8rem" };
// Tightly-stacked targeting checkboxes need a ≥24px row so adjacent checkbox
// centers clear WCAG 2.5.8 target-size (they have no inter-row gap).
const condCheck = { display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem", minHeight: "1.625rem" };
const mutedType = { color: "var(--color-fg-muted)", fontSize: "0.72rem", fontFamily: "var(--font-mono, monospace)" };
