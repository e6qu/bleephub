// API latency benchmark against a seeded local Bleephub (scripts/perf/seed.sh).
// Prints p50/p95/p99 and throughput per endpoint at c=1 and c=20.
//
// The PAT-authenticated endpoints consume the GitHub-parity core budget
// (5,000/hour per credential); the default request counts stay under it, but a
// second back-to-back run may 403 — restart the server to reset the window, or
// lower REQS. Search endpoints are excluded: their 30/min budget makes latency
// sampling meaningless.
//
//   node scripts/perf/bench.mjs
//   BLEEPHUB_BASE=http://localhost:15599 REQS=250 node scripts/perf/bench.mjs

const BASE = process.env.BLEEPHUB_BASE || "http://localhost:15599";
const TOKEN = process.env.BLEEPHUB_TOKEN || "bleephub-admin-token-00000000000000000000";
const REQS = Number(process.env.REQS || 250);
const H = { Authorization: `token ${TOKEN}` };

const endpoints = [
  ["repo object", "/api/v3/repos/admin/hello-app"],
  ["issues list (50)", "/api/v3/repos/admin/hello-app/issues?per_page=50"],
  ["issues list (100, all)", "/api/v3/repos/admin/hello-app/issues?per_page=100&state=all"],
  ["commits list (50)", "/api/v3/repos/admin/hello-app/commits?per_page=50"],
  ["tree recursive", "/api/v3/repos/admin/hello-app/git/trees/main?recursive=1"],
  ["pr files", "/api/v3/repos/admin/hello-app/pulls/121/files"],
  ["spa shell /ui/", "/ui/"],
];

function pct(a, p) {
  const s = [...a].sort((x, y) => x - y);
  return s[Math.min(s.length - 1, Math.floor(p * s.length))];
}

async function run(name, path, conc, total) {
  const times = [];
  let inflight = 0;
  let done = 0;
  let errors = 0;
  const t0 = performance.now();
  await new Promise((resolve) => {
    const kick = () => {
      while (inflight < conc && done + inflight < total) {
        inflight++;
        const s = performance.now();
        fetch(BASE + path, { headers: H })
          .then(async (r) => {
            await r.arrayBuffer();
            if (r.status >= 400) errors++;
            times.push(performance.now() - s);
          })
          .catch(() => errors++)
          .finally(() => {
            inflight--;
            done++;
            if (done >= total) resolve();
            else kick();
          });
      }
    };
    kick();
  });
  const wall = (performance.now() - t0) / 1000;
  const row = [
    name.padEnd(26),
    `c=${String(conc).padStart(2)}`,
    `p50=${pct(times, 0.5).toFixed(1).padStart(6)}ms`,
    `p95=${pct(times, 0.95).toFixed(1).padStart(6)}ms`,
    `p99=${pct(times, 0.99).toFixed(1).padStart(6)}ms`,
    `rps=${String(Math.round(total / wall)).padStart(6)}`,
    `err=${errors}`,
  ].join("  ");
  console.log(row);
  return errors;
}

const health = await fetch(`${BASE}/health`).catch(() => null);
if (!health?.ok) {
  console.error(`no server at ${BASE} — start one and run scripts/perf/seed.sh first`);
  process.exit(1);
}

let totalErrors = 0;
for (const [name, path] of endpoints) {
  totalErrors += await run(name, path, 1, Math.min(REQS, 100));
  totalErrors += await run(name, path, 20, REQS);
}
if (totalErrors > 0) {
  console.error(`\n${totalErrors} responses were >=400 — likely the core rate budget; restart the server and rerun with lower REQS.`);
  process.exit(1);
}
