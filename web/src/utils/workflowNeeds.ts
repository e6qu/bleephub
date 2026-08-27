import { parse } from "yaml";

export interface WorkflowJobSpec {
  /** YAML key under `jobs:`. */
  key: string;
  name?: string;
  /** `needs`, normalized to a list of YAML keys. */
  needs: string[];
}

/**
 * Parse each job's `needs` from workflow YAML — the only client-visible source
 * of dependency info (the jobs REST payload has none). Best-effort: bad YAML
 * yields an empty list, never a crash.
 */
export function parseWorkflowJobSpecs(yamlText: string): WorkflowJobSpec[] {
  let doc: unknown;
  try {
    doc = parse(yamlText);
  } catch {
    return [];
  }
  if (doc === null || typeof doc !== "object") return [];
  const jobs = (doc as Record<string, unknown>).jobs;
  if (jobs === null || typeof jobs !== "object" || Array.isArray(jobs)) return [];
  const specs: WorkflowJobSpec[] = [];
  for (const [key, raw] of Object.entries(jobs as Record<string, unknown>)) {
    const spec: WorkflowJobSpec = { key, needs: [] };
    if (raw !== null && typeof raw === "object") {
      const job = raw as Record<string, unknown>;
      if (typeof job.name === "string") spec.name = job.name;
      if (typeof job.needs === "string") spec.needs = [job.needs];
      else if (Array.isArray(job.needs)) {
        spec.needs = job.needs.filter((n): n is string => typeof n === "string");
      }
    }
    specs.push(spec);
  }
  return specs;
}

/** Display name for a YAML job key: its `name:` when set, else the key. */
function displayNameFor(specs: WorkflowJobSpec[], key: string): string {
  const spec = specs.find((s) => s.key === key);
  return spec?.name ?? key;
}

/**
 * Resolve a job's `needs` (as display names) from its REST `jobName`, matching
 * the YAML job by `name:` or key, including the `name (matrix, …)` expansion.
 */
export function needsForJobName(specs: WorkflowJobSpec[], jobName: string): string[] {
  const spec =
    specs.find((s) => (s.name ?? s.key) === jobName) ??
    specs.find((s) => jobName.startsWith(`${s.name ?? s.key} (`));
  if (!spec) return [];
  return spec.needs.map((key) => displayNameFor(specs, key));
}
