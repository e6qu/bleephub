import { parse } from "yaml";
import type { WorkflowDispatchInput } from "../types.js";

export interface WorkflowDispatchSpec {
  hasDispatch: boolean;
  // Input name → definition, in YAML declaration order.
  inputs: Record<string, WorkflowDispatchInput>;
}

function normalizeInput(raw: unknown): WorkflowDispatchInput {
  if (raw === null || typeof raw !== "object") return {};
  const r = raw as Record<string, unknown>;
  const input: WorkflowDispatchInput = {};
  if (typeof r.description === "string") input.description = r.description;
  if (typeof r.required === "boolean") input.required = r.required;
  if (typeof r.default === "string" || typeof r.default === "boolean") input.default = r.default;
  if (typeof r.default === "number") input.default = String(r.default);
  if (
    r.type === "string" ||
    r.type === "choice" ||
    r.type === "boolean" ||
    r.type === "environment" ||
    r.type === "number"
  ) {
    input.type = r.type;
  }
  if (Array.isArray(r.options)) {
    input.options = r.options.map((o) => String(o));
  }
  return input;
}

// YAML 1.1 parsers coerce the bare `on` key to boolean true (the "true" key), so accept both.
export function parseWorkflowDispatch(yamlText: string): WorkflowDispatchSpec {
  let doc: unknown;
  try {
    doc = parse(yamlText);
  } catch {
    return { hasDispatch: false, inputs: {} };
  }
  if (doc === null || typeof doc !== "object") return { hasDispatch: false, inputs: {} };
  const root = doc as Record<string, unknown>;
  const on = root.on ?? root.true;
  if (on === undefined || on === null) return { hasDispatch: false, inputs: {} };

  if (typeof on === "string") {
    return { hasDispatch: on === "workflow_dispatch", inputs: {} };
  }
  if (Array.isArray(on)) {
    return { hasDispatch: on.includes("workflow_dispatch"), inputs: {} };
  }
  if (typeof on === "object") {
    const events = on as Record<string, unknown>;
    if (!("workflow_dispatch" in events)) return { hasDispatch: false, inputs: {} };
    const wd = events.workflow_dispatch;
    const inputs: Record<string, WorkflowDispatchInput> = {};
    if (wd !== null && typeof wd === "object") {
      const rawInputs = (wd as Record<string, unknown>).inputs;
      if (rawInputs !== null && typeof rawInputs === "object" && !Array.isArray(rawInputs)) {
        for (const [name, def] of Object.entries(rawInputs as Record<string, unknown>)) {
          inputs[name] = normalizeInput(def);
        }
      }
    }
    return { hasDispatch: true, inputs };
  }
  return { hasDispatch: false, inputs: {} };
}
