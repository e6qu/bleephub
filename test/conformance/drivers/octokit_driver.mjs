// Conformance driver for octokit.js, the official JavaScript client.
//
// Octokit is the harness's pagination and GraphQL oracle: octokit.paginate only
// works if the server sends an RFC 5988 Link header the client can follow, and
// octokit.graphql only works if the GraphQL endpoint answers with the
// {data, errors} envelope at the Enterprise path. A row passes only when the
// client's own helper produced the right value, never merely because a request
// returned 200.
//
// The client is resolved out of web/node_modules, which the repository already
// pins (octokit is a devDependency of web/package.json and is covered by
// scripts/check-dependency-age.py through bun's lockfile). Nothing is fetched
// at run time.

import { readFileSync, writeFileSync, appendFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..", "..");
const octokitDir = join(repoRoot, "web", "node_modules", "octokit");
const manifest = JSON.parse(readFileSync(join(octokitDir, "package.json"), "utf8"));
const entry =
  manifest.exports?.["."]?.import ??
  manifest.exports?.["."]?.default ??
  manifest.module ??
  manifest.main;
const { Octokit } = await import(join(octokitDir, entry));

const base = (process.env.BPH_BASE ?? "").replace(/\/$/, "");
const token = process.env.BPH_TOKEN ?? "";
const resultsPath = process.env.BPH_RESULTS ?? "";
if (!base || !token) {
  console.error("BPH_BASE and BPH_TOKEN are required");
  process.exit(2);
}
if (resultsPath) writeFileSync(resultsPath, "");

let passed = 0;
let failed = 0;
let skipped = 0;

const truncate = (value) => {
  const text = String(value ?? "").replace(/\s+/g, " ").trim();
  return text.length > 400 ? `${text.slice(0, 400)}…` : text;
};

function emit(record) {
  const line = `${JSON.stringify({ client: "octokit", ...record })}\n`;
  if (resultsPath) appendFileSync(resultsPath, line);
  else process.stdout.write(line);
}

// A Deviation is what a driver raises when the transport succeeded but the
// decoded value does not satisfy the operation's contract.
class Deviation extends Error {
  constructor(expected, actual, message) {
    super(message ?? `expected ${expected}, got ${actual}`);
    this.expected = expected;
    this.actual = actual;
  }
}

async function check(domain, operation, request, fn) {
  try {
    await fn();
    passed += 1;
    emit({ domain, operation, status: "pass", request });
  } catch (error) {
    failed += 1;
    if (error instanceof Deviation) {
      emit({
        domain,
        operation,
        status: "fail",
        request,
        expected: truncate(error.expected),
        actual: truncate(error.actual),
        message: truncate(error.message),
      });
      return;
    }
    const detail =
      error?.status !== undefined
        ? `HTTP ${error.status}: ${truncate(JSON.stringify(error.response?.data ?? error.message))}`
        : truncate(error?.stack ?? error?.message ?? error);
    emit({
      domain,
      operation,
      status: "fail",
      request,
      expected: "the client call resolves and the value satisfies the contract",
      actual: detail,
      message: detail,
    });
  }
}

function skip(domain, operation, request, why) {
  skipped += 1;
  emit({ domain, operation, status: "skip", request, message: why });
}

function want(condition, expected, actual, message) {
  if (!condition) throw new Deviation(expected, actual, message);
}

const octokit = new Octokit({ auth: token, baseUrl: `${base}/api/v3` });

const owner = "admin";
const repo = "conformance-octokit";
let issueNumber = 0;

// --- Fixtures ---------------------------------------------------------------
await check("repos", "repos.createForAuthenticatedUser", "POST /user/repos", async () => {
  const { data } = await octokit.rest.repos.createForAuthenticatedUser({
    name: repo,
    description: "octokit conformance fixture",
    auto_init: true,
  });
  want(data.full_name === `${owner}/${repo}`, `${owner}/${repo}`, data.full_name, "full_name is wrong");
  want(typeof data.id === "number", "a numeric id", typeof data.id, "id is not a number");
  want(Boolean(data.node_id), "a node_id", "empty", "the repository has no node_id");
});

await check("issues", "issues.create", "POST /repos/{owner}/{repo}/issues", async () => {
  const { data } = await octokit.rest.issues.create({
    owner,
    repo,
    title: "octokit conformance issue",
    body: "opened by the octokit conformance driver",
  });
  issueNumber = data.number;
  want(data.number > 0, "a positive number", data.number, "the created issue has no number");
  want(data.state === "open", "open", data.state, "the created issue is not open");
});

// --- Core REST surface ------------------------------------------------------
await check("users", "users.getAuthenticated", "GET /user", async () => {
  const { data } = await octokit.rest.users.getAuthenticated();
  want(data.login === owner, owner, data.login, "the authenticated login is wrong");
  want(data.type === "User", "User", data.type, "the authenticated user type is wrong");
});

await check("repos", "repos.get", "GET /repos/{owner}/{repo}", async () => {
  const { data } = await octokit.rest.repos.get({ owner, repo });
  want(Boolean(data.clone_url), "clone_url", "empty", "the repository has no clone_url");
  want(Boolean(data.owner?.avatar_url !== undefined), "owner.avatar_url present", "absent", "owner is not expanded");
  want(data.permissions !== undefined, "a permissions object", "absent", "the repository has no permissions object");
});

await check("repos", "repos.listForAuthenticatedUser", "GET /user/repos", async () => {
  const { data } = await octokit.rest.repos.listForAuthenticatedUser();
  want(Array.isArray(data) && data.length > 0, "a non-empty array", `${data.length} items`, "the listing is empty");
});

await check("repos", "repos.listBranches", "GET /repos/{owner}/{repo}/branches", async () => {
  const { data } = await octokit.rest.repos.listBranches({ owner, repo });
  want(data.length > 0, "at least the default branch", "0 branches", "the branch listing is empty");
  want(Boolean(data[0].commit?.sha), "branch.commit.sha", "empty", "the listed branch has no commit sha");
});

await check("repos", "repos.getContent (file)", "GET /repos/{owner}/{repo}/contents/{path}", async () => {
  const { data } = await octokit.rest.repos.getContent({ owner, repo, path: "README.md" });
  want(!Array.isArray(data), "a file object", "an array", "a file path returned a directory listing");
  want(data.encoding === "base64", "base64", data.encoding, "content is not base64 encoded");
  const decoded = Buffer.from(data.content, "base64").toString("utf8");
  want(decoded.length > 0, "decodable content", "empty", "the base64 content decodes to nothing");
});

await check("repos", "repos.getContent (raw media type)", "GET contents with Accept: application/vnd.github.raw", async () => {
  const response = await octokit.request("GET /repos/{owner}/{repo}/contents/{path}", {
    owner,
    repo,
    path: "README.md",
    headers: { accept: "application/vnd.github.raw" },
  });
  // Octokit decides how to hand the body to the caller from the response's
  // Content-Type: text/* becomes a string, anything else becomes a binary
  // buffer. So the header, not just the body, decides whether a caller asking
  // for raw file content gets text back.
  want(
    typeof response.data === "string",
    "a raw string body (Content-Type: text/plain; charset=utf-8, as GitHub sends)",
    `${typeof response.data} (Content-Type: ${response.headers["content-type"]})`,
    "the raw media type produced a non-text Content-Type, so octokit hands the caller a binary buffer instead of the file's text",
  );
  want(
    (response.headers["x-github-media-type"] ?? "").includes("raw"),
    "x-github-media-type naming the raw format",
    response.headers["x-github-media-type"] ?? "absent",
    "the response still reports format=json after a raw media type was requested",
  );
});

await check("repos", "repos.createOrUpdateFileContents", "PUT /repos/{owner}/{repo}/contents/{path}", async () => {
  const { data } = await octokit.rest.repos.createOrUpdateFileContents({
    owner,
    repo,
    path: "octokit.txt",
    message: "add an octokit fixture file",
    content: Buffer.from("octokit conformance\n").toString("base64"),
  });
  want(Boolean(data.commit?.sha), "commit.sha", "empty", "the write response has no commit sha");
  want(Boolean(data.content?.html_url), "content.html_url", "empty", "the write response has no content html_url");
});

await check("issues", "issues.get", "GET /repos/{owner}/{repo}/issues/{issue_number}", async () => {
  const { data } = await octokit.rest.issues.get({ owner, repo, issue_number: issueNumber });
  want(Boolean(data.html_url), "html_url", "empty", "the issue has no html_url");
  want(data.reactions !== undefined, "a reactions summary", "absent", "the issue has no reactions summary");
  want(Boolean(data.author_association), "author_association", "empty", "the issue has no author_association");
});

await check("issues", "issues.createComment", "POST /repos/{owner}/{repo}/issues/{n}/comments", async () => {
  const { data } = await octokit.rest.issues.createComment({
    owner,
    repo,
    issue_number: issueNumber,
    body: "octokit conformance comment",
  });
  want(data.id > 0, "a comment id", data.id, "the created comment has no id");
});

await check("issues", "issues.listForRepo", "GET /repos/{owner}/{repo}/issues", async () => {
  const { data } = await octokit.rest.issues.listForRepo({ owner, repo });
  want(data.length > 0, "at least one issue", "0 issues", "the issue listing is empty");
});

await check("issues", "reactions.createForIssue", "POST /repos/{owner}/{repo}/issues/{n}/reactions", async () => {
  const { data } = await octokit.rest.reactions.createForIssue({
    owner,
    repo,
    issue_number: issueNumber,
    content: "rocket",
  });
  want(data.content === "rocket", "rocket", data.content, "the reaction content is wrong");
});

await check("search", "search.repos", "GET /search/repositories", async () => {
  const { data } = await octokit.rest.search.repos({ q: "conformance" });
  want(typeof data.total_count === "number", "a numeric total_count", typeof data.total_count, "total_count is missing");
  want(Array.isArray(data.items), "an items array", typeof data.items, "items is missing");
});

await check("actions", "actions.listRepoWorkflows", "GET /repos/{owner}/{repo}/actions/workflows", async () => {
  const { data } = await octokit.rest.actions.listRepoWorkflows({ owner, repo });
  want(typeof data.total_count === "number", "a numeric total_count", typeof data.total_count,
    "the workflow listing is not wrapped in the {total_count, workflows} envelope");
  want(Array.isArray(data.workflows), "a workflows array", typeof data.workflows, "workflows is missing");
});

await check("releases", "repos.createRelease", "POST /repos/{owner}/{repo}/releases", async () => {
  const { data } = await octokit.rest.repos.createRelease({ owner, repo, tag_name: "octokit-v1" });
  want(Boolean(data.upload_url), "upload_url", "empty", "the release has no upload_url");
  want(data.upload_url.includes("{"), "an RFC 6570 templated upload_url", data.upload_url,
    "upload_url is not a URI template, so clients cannot expand it");
});

await check("meta", "meta.get", "GET /meta", async () => {
  const { data } = await octokit.rest.meta.get();
  want(data.verifiable_password_authentication !== undefined,
    "verifiable_password_authentication", "absent", "/meta is missing a documented field");
});

await check("meta", "rateLimit.get", "GET /rate_limit", async () => {
  const { data } = await octokit.rest.rateLimit.get();
  want(data.resources?.core?.limit > 0, "resources.core.limit > 0", data.resources?.core?.limit,
    "the rate limit envelope is missing resources.core");
});

// --- Pagination -------------------------------------------------------------
await check("pagination", "octokit.paginate over issues", "GET /repos/{owner}/{repo}/issues?per_page=1", async () => {
  for (let index = 0; index < 4; index += 1) {
    await octokit.rest.issues.create({ owner, repo, title: `pagination fixture ${index}` });
  }
  const all = await octokit.paginate(octokit.rest.issues.listForRepo, {
    owner,
    repo,
    per_page: 2,
    state: "all",
  });
  want(all.length >= 5, "at least the 5 issues created", `${all.length} issues`,
    "octokit.paginate could not walk past the first page, which means the Link header is missing or wrong");
});

await check("pagination", "octokit.paginate.iterator", "GET /repos/{owner}/{repo}/issues?per_page=2", async () => {
  let pages = 0;
  let items = 0;
  for await (const response of octokit.paginate.iterator(octokit.rest.issues.listForRepo, {
    owner,
    repo,
    per_page: 2,
    state: "all",
  })) {
    pages += 1;
    items += response.data.length;
    if (pages > 10) break;
  }
  want(pages >= 2, "more than one page at per_page=2", `${pages} page(s)`,
    "the iterator stopped after one page, so the Link header does not advertise a next page");
  want(items >= 5, "every issue across the pages", `${items} issues`, "pages lost items");
});

await check("pagination", "Link header rel=next/last", "GET /repos/{owner}/{repo}/issues?per_page=2", async () => {
  const response = await octokit.request("GET /repos/{owner}/{repo}/issues", {
    owner,
    repo,
    per_page: 2,
    state: "all",
  });
  const link = response.headers.link ?? "";
  want(link.includes('rel="next"'), 'a Link header with rel="next"', link || "no Link header",
    "the response carries no Link header, so every client's pagination stops at page one");
});

await check("pagination", "octokit.paginate over search results", "GET /search/issues?per_page=1", async () => {
  const all = await octokit.paginate(octokit.rest.search.issuesAndPullRequests, {
    q: "pagination",
    per_page: 1,
  });
  const { data } = await octokit.rest.search.issuesAndPullRequests({ q: "pagination", per_page: 1 });
  want(data.total_count > 1, "a search matching more than one issue", data.total_count,
    "the fixture did not produce enough search matches to page over");
  want(all.length === data.total_count, `${data.total_count} results across pages`, all.length,
    "octokit.paginate could not walk the search results, which means search responses carry no Link header");
});

// --- GraphQL ----------------------------------------------------------------
await check("graphql", "graphql viewer query", "POST /api/graphql", async () => {
  const result = await octokit.graphql(`query { viewer { login } }`);
  want(result?.viewer?.login === owner, owner, JSON.stringify(result), "the viewer query returned the wrong login");
});

await check("graphql", "graphql repository query with variables", "POST /api/graphql", async () => {
  const result = await octokit.graphql(
    `query ($owner: String!, $name: String!) {
       repository(owner: $owner, name: $name) { id name nameWithOwner defaultBranchRef { name } }
     }`,
    { owner, name: repo },
  );
  want(result?.repository?.nameWithOwner === `${owner}/${repo}`, `${owner}/${repo}`,
    JSON.stringify(result?.repository), "the repository query returned the wrong node");
  want(Boolean(result?.repository?.id), "a global node id", "empty", "the repository node has no id");
});

await check("graphql", "graphql issues connection", "POST /api/graphql", async () => {
  const result = await octokit.graphql(
    `query ($owner: String!, $name: String!) {
       repository(owner: $owner, name: $name) {
         issues(first: 2) {
           totalCount
           pageInfo { hasNextPage endCursor }
           nodes { number title }
         }
       }
     }`,
    { owner, name: repo },
  );
  const issues = result?.repository?.issues;
  want(typeof issues?.totalCount === "number", "a numeric totalCount", typeof issues?.totalCount,
    "the issues connection has no totalCount");
  want(issues?.pageInfo?.hasNextPage !== undefined, "pageInfo.hasNextPage", "absent",
    "the connection has no pageInfo, so cursor pagination is impossible");
});

await check("graphql", "graphql mutation (addComment)", "POST /api/graphql", async () => {
  const issue = await octokit.graphql(
    `query ($owner: String!, $name: String!, $number: Int!) {
       repository(owner: $owner, name: $name) { issue(number: $number) { id } }
     }`,
    { owner, name: repo, number: issueNumber },
  );
  const subjectId = issue?.repository?.issue?.id;
  want(Boolean(subjectId), "an issue node id", "empty", "the issue node has no id to mutate");
  const result = await octokit.graphql(
    `mutation ($subjectId: ID!, $body: String!) {
       addComment(input: {subjectId: $subjectId, body: $body}) {
         commentEdge { node { body } }
       }
     }`,
    { subjectId, body: "octokit graphql comment" },
  );
  want(result?.addComment?.commentEdge?.node?.body === "octokit graphql comment",
    "the comment body echoed back", JSON.stringify(result?.addComment), "the mutation did not return the created comment");
});

// Linked branches: the association behind GitHub's "create a branch" control
// on an issue, and the capability `gh issue develop` drives on github.com. The
// gh subcommand refuses any host but github.com, so this is where the
// capability is exercised — the whole lifecycle in one operation, because a
// link that cannot be listed or removed is not a link.
await check("graphql", "graphql linked branches (createLinkedBranch / linkedBranches / deleteLinkedBranch)",
  "POST /api/graphql", async () => {
    const seed = await octokit.graphql(
      `query ($owner: String!, $name: String!, $number: Int!) {
         repository(owner: $owner, name: $name) {
           issue(number: $number) { id number linkedBranches(first: 10) { totalCount nodes { id } } }
           defaultBranchRef { target { oid } }
         }
       }`,
      { owner, name: repo, number: issueNumber },
    );
    const issueId = seed?.repository?.issue?.id;
    const oid = seed?.repository?.defaultBranchRef?.target?.oid;
    want(Boolean(issueId), "an issue node id", "empty", "the issue node has no id to link a branch to");
    want(Boolean(oid), "the default branch head oid", "empty",
      "the default branch has no target commit to base a linked branch on");
    want(seed?.repository?.issue?.linkedBranches?.totalCount === 0, "an empty linkedBranches connection",
      JSON.stringify(seed?.repository?.issue?.linkedBranches),
      "an issue nothing has linked a branch to already reports linked branches");

    const branch = `${issueNumber}-octokit-linked-branch`;
    const created = await octokit.graphql(
      `mutation ($issueId: ID!, $oid: GitObjectID!, $name: String!) {
         createLinkedBranch(input: {issueId: $issueId, oid: $oid, name: $name}) {
           linkedBranch { id ref { name prefix target { oid } repository { nameWithOwner } } }
           issue { number }
         }
       }`,
      { issueId, oid, name: branch },
    );
    const linked = created?.createLinkedBranch?.linkedBranch;
    want(Boolean(linked?.id), "a linked branch node id", JSON.stringify(created?.createLinkedBranch),
      "createLinkedBranch returned no linked branch");
    want(linked?.ref?.name === branch, branch, String(linked?.ref?.name),
      "the linked branch does not name the branch that was asked for");
    want(linked?.ref?.target?.oid === oid, oid, String(linked?.ref?.target?.oid),
      "the linked branch does not point at the commit it was based on");
    want(linked?.ref?.repository?.nameWithOwner === `${owner}/${repo}`,
      `${owner}/${repo}`, String(linked?.ref?.repository?.nameWithOwner),
      "the linked branch is attributed to the wrong repository");

    // The branch is a real reference, not merely a record: a client that reads
    // it over the git data API has to see it.
    const ref = await octokit.rest.git.getRef({ owner, repo, ref: `heads/${branch}` });
    want(ref.data.object?.sha === oid, oid, String(ref.data.object?.sha),
      "createLinkedBranch did not create the branch it linked");

    const listed = await octokit.graphql(
      `query ($owner: String!, $name: String!, $number: Int!) {
         repository(owner: $owner, name: $name) {
           issue(number: $number) {
             linkedBranches(first: 10) {
               totalCount
               nodes { id ref { name } }
               edges { cursor node { id } }
               pageInfo { hasNextPage endCursor }
             }
           }
         }
       }`,
      { owner, name: repo, number: issueNumber },
    );
    const connection = listed?.repository?.issue?.linkedBranches;
    want(connection?.totalCount === 1, "totalCount 1", String(connection?.totalCount),
      "the linked branch is absent from the issue's linkedBranches connection");
    want(connection?.nodes?.[0]?.id === linked.id, linked.id, String(connection?.nodes?.[0]?.id),
      "the listed linked branch is not the one that was created");
    want(connection?.nodes?.[0]?.ref?.name === branch, branch, String(connection?.nodes?.[0]?.ref?.name),
      "the listed linked branch names a different ref");
    want(Boolean(connection?.edges?.[0]?.cursor), "an edge cursor", "empty",
      "the connection has no cursor, so a client cannot paginate it");

    const refetched = await octokit.graphql(
      `query ($id: ID!) { node(id: $id) { __typename ... on LinkedBranch { ref { name } } } }`,
      { id: linked.id },
    );
    want(refetched?.node?.__typename === "LinkedBranch", "LinkedBranch",
      String(refetched?.node?.__typename), "a linked branch id does not refetch through node()");
    want(refetched?.node?.ref?.name === branch, branch, String(refetched?.node?.ref?.name),
      "the refetched linked branch names a different ref");

    await octokit.graphql(
      `mutation ($id: ID!) { deleteLinkedBranch(input: {linkedBranchId: $id}) { issue { number } } }`,
      { id: linked.id },
    );
    const after = await octokit.graphql(
      `query ($owner: String!, $name: String!, $number: Int!) {
         repository(owner: $owner, name: $name) {
           issue(number: $number) { linkedBranches(first: 10) { totalCount } }
         }
       }`,
      { owner, name: repo, number: issueNumber },
    );
    want(after?.repository?.issue?.linkedBranches?.totalCount === 0, "totalCount 0",
      String(after?.repository?.issue?.linkedBranches?.totalCount),
      "unlinking left the branch linked to the issue");
    // Unlinking removes the association only; GitHub leaves the branch alone.
    const survivor = await octokit.rest.git.getRef({ owner, repo, ref: `heads/${branch}` });
    want(survivor.data.object?.sha === oid, oid, String(survivor.data.object?.sha),
      "unlinking a branch deleted the branch itself");
  });

await check("graphql", "graphql error envelope", "POST /api/graphql with an invalid field", async () => {
  try {
    await octokit.graphql(`query { viewer { thisFieldDoesNotExist } }`);
  } catch (error) {
    want(Array.isArray(error.errors) && error.errors.length > 0, "a GraphQL errors array",
      truncate(error.message), "an invalid query did not produce the {errors:[...]} envelope the client parses");
    return;
  }
  throw new Deviation("a GraphQL error", "success", "an invalid field was accepted");
});

await check("graphql", "graphql node/global id round trip", "POST /api/graphql", async () => {
  const viewer = await octokit.graphql(`query { viewer { id login } }`);
  const id = viewer?.viewer?.id;
  want(Boolean(id), "a viewer node id", "empty", "the viewer has no global node id");
  const result = await octokit.graphql(`query ($id: ID!) { node(id: $id) { __typename ... on User { login } } }`, { id });
  want(result?.node?.login === owner, owner, JSON.stringify(result?.node),
    "node(id:) could not resolve the viewer's own global id");
});

// --- Error handling ---------------------------------------------------------
await check("errors", "RequestError on 404", "GET /repos/{owner}/does-not-exist", async () => {
  try {
    await octokit.rest.repos.get({ owner, repo: "definitely-does-not-exist" });
  } catch (error) {
    want(error.status === 404, "status 404", error.status, "a missing repository does not answer 404");
    want(Boolean(error.response?.data?.message), "a message in the error body", "absent",
      "the error body has no message, so the client cannot explain the failure");
    return;
  }
  throw new Deviation("a 404", "success", "a missing repository was served successfully");
});

await check("errors", "RequestError on 422", "POST /repos/{owner}/{repo}/issues without a title", async () => {
  try {
    await octokit.request("POST /repos/{owner}/{repo}/issues", { owner, repo, title: "" });
  } catch (error) {
    want(error.status === 422, "status 422", error.status, "an invalid issue body was not rejected with 422");
    return;
  }
  throw new Deviation("a 422", "success", "an empty issue title was accepted");
});

// --- Transport details a client depends on ----------------------------------
await check("transport", "x-ratelimit headers", "GET /user", async () => {
  const response = await octokit.request("GET /user");
  want(response.headers["x-ratelimit-limit"] !== undefined, "an x-ratelimit-limit header", "absent",
    "responses carry no rate-limit headers, so the throttling plugin cannot pace itself");
  want(response.headers["x-ratelimit-remaining"] !== undefined, "an x-ratelimit-remaining header", "absent",
    "responses carry no x-ratelimit-remaining header");
});

await check("transport", "x-github-media-type / api version headers", "GET /user", async () => {
  const response = await octokit.request("GET /user");
  want(
    response.headers["x-github-media-type"] !== undefined ||
      response.headers["x-github-api-version-selected"] !== undefined,
    "an x-github-media-type or x-github-api-version-selected header",
    "neither present",
    "the response carries none of GitHub's media-type headers",
  );
});

await check("transport", "ETag and 304", "GET /user twice", async () => {
  const first = await octokit.request("GET /user");
  const etag = first.headers.etag;
  want(Boolean(etag), "an ETag header", "absent", "responses carry no ETag, so conditional requests are impossible");
  try {
    const second = await octokit.request("GET /user", { headers: { "if-none-match": etag } });
    throw new Deviation("304 Not Modified", `${second.status}`, "a matching ETag did not produce 304");
  } catch (error) {
    if (error instanceof Deviation) throw error;
    want(error.status === 304, "304 Not Modified", error.status, "a matching ETag did not produce 304");
  }
});


// --- GraphQL, in the depth a real client uses it -----------------------------
// The queries below are the shapes clients actually send: fragments and
// aliases, connections walked by cursor, node/nodes refetch by global id,
// mutations carrying clientMutationId, and errors that must arrive as a 200
// with an errors array rather than as a transport failure.

await check("graphql", "graphql query with fragments and aliases", "POST /api/graphql", async () => {
  const result = await octokit.graphql(
    `query ($owner: String!, $name: String!) {
       primary: repository(owner: $owner, name: $name) { ...repoFields }
       viewer { login }
     }
     fragment repoFields on Repository {
       __typename
       id
       name
       nameWithOwner
       isPrivate
       owner { __typename login }
     }`,
    { owner, name: repo },
  );
  want(result?.primary?.__typename === "Repository", "Repository", result?.primary?.__typename,
    "__typename is not resolved, so a client cannot discriminate a union or interface");
  want(result?.primary?.nameWithOwner === `${owner}/${repo}`, `${owner}/${repo}`,
    JSON.stringify(result?.primary), "the aliased field did not resolve to the repository");
  want(result?.primary?.owner?.__typename === "User" || result?.primary?.owner?.__typename === "Organization",
    "User or Organization", result?.primary?.owner?.__typename,
    "the repository owner does not resolve to a concrete type");
});

await check("graphql", "graphql nodes() batch refetch", "POST /api/graphql", async () => {
  const seed = await octokit.graphql(
    `query ($owner: String!, $name: String!) {
       repository(owner: $owner, name: $name) { id issues(first: 1) { nodes { id } } }
     }`,
    { owner, name: repo },
  );
  const repositoryId = seed?.repository?.id;
  const issueId = seed?.repository?.issues?.nodes?.[0]?.id;
  want(Boolean(repositoryId) && Boolean(issueId), "a repository id and an issue id",
    JSON.stringify(seed), "the seed query did not return the global ids to refetch");
  const result = await octokit.graphql(
    `query ($ids: [ID!]!) {
       nodes(ids: $ids) {
         __typename
         ... on Repository { nameWithOwner }
         ... on Issue { number }
       }
     }`,
    { ids: [repositoryId, issueId] },
  );
  want(Array.isArray(result?.nodes) && result.nodes.length === 2, "two nodes",
    JSON.stringify(result?.nodes), "nodes(ids:) did not return one node per identifier");
  const types = result.nodes.map((node) => node?.__typename).sort();
  want(types.join(",") === "Issue,Repository", "Issue,Repository", types.join(","),
    "nodes(ids:) resolved the identifiers to the wrong types");
});

await check("graphql", "graphql node() on an unknown id is a null node, not a transport error",
  "POST /api/graphql", async () => {
    const result = await octokit.graphql(
      `query { node(id: "MDQ6VXNlcjk5OTk5OTk5OTk=") { __typename } }`,
    ).catch((error) => error);
    if (result instanceof Error) {
      want(Array.isArray(result.errors), "a GraphQL errors array", truncate(result.message),
        "an unresolvable node id produced a transport failure rather than a GraphQL error");
      want(result.status === undefined || result.status === 200, "HTTP 200 carrying the error",
        result.status, "a GraphQL error was returned with a non-200 status");
      return;
    }
    want(result?.node === null, "node: null", JSON.stringify(result),
      "an unresolvable node id resolved to something");
  });

await check("graphql", "graphql mutation echoes clientMutationId", "POST /api/graphql", async () => {
  const seed = await octokit.graphql(
    `query ($owner: String!, $name: String!, $number: Int!) {
       repository(owner: $owner, name: $name) { issue(number: $number) { id } }
     }`,
    { owner, name: repo, number: issueNumber },
  );
  const subjectId = seed?.repository?.issue?.id;
  want(Boolean(subjectId), "an issue node id", "empty", "the issue node has no id to mutate");
  const result = await octokit.graphql(
    `mutation ($subjectId: ID!, $body: String!, $token: String!) {
       addComment(input: {subjectId: $subjectId, body: $body, clientMutationId: $token}) {
         clientMutationId
         commentEdge { node { id body author { login } } }
       }
     }`,
    { subjectId, body: "clientMutationId round trip", token: "conformance-mutation-1" },
  );
  want(result?.addComment?.clientMutationId === "conformance-mutation-1",
    "conformance-mutation-1", JSON.stringify(result?.addComment?.clientMutationId),
    "clientMutationId is not echoed, so a client cannot correlate a mutation with its own request");
  want(Boolean(result?.addComment?.commentEdge?.node?.id), "a comment node id", "empty",
    "the mutation payload carries no node id");
  want(result?.addComment?.commentEdge?.node?.author?.login === owner, owner,
    JSON.stringify(result?.addComment?.commentEdge?.node?.author),
    "the created comment has no author");
});

await check("graphql", "graphql mutation validation error is a 200 with errors[]",
  "POST /api/graphql with an unusable subject id", async () => {
    try {
      await octokit.graphql(
        `mutation ($subjectId: ID!) {
           addComment(input: {subjectId: $subjectId, body: "no"}) { clientMutationId }
         }`,
        { subjectId: "not-a-real-global-id" },
      );
    } catch (error) {
      want(Array.isArray(error.errors) && error.errors.length > 0, "a GraphQL errors array",
        truncate(error.message), "a bad mutation input did not produce the {errors:[...]} envelope");
      want(error.status === undefined || error.status === 200, "HTTP 200 carrying the error",
        error.status,
        "a GraphQL mutation error arrived as an HTTP error status, which every GraphQL client treats as a transport fault");
      return;
    }
    throw new Deviation("a GraphQL error", "success", "a mutation with an unusable subject id succeeded");
  });

await check("graphql", "graphql variable type mismatch is a GraphQL error", "POST /api/graphql", async () => {
  try {
    await octokit.graphql(
      `query ($number: Int!) { repository(owner: "${owner}", name: "${repo}") { issue(number: $number) { title } } }`,
      { number: "not-a-number" },
    );
  } catch (error) {
    want(Array.isArray(error.errors) && error.errors.length > 0, "a GraphQL errors array",
      truncate(error.message), "a variable of the wrong type did not produce a GraphQL error");
    return;
  }
  throw new Deviation("a GraphQL error", "success", "a string was accepted where Int! was declared");
});

await check("graphql", "graphql cursor pagination over a connection", "POST /api/graphql", async () => {
  // Seed enough issues that the connection genuinely has more than one page.
  for (let index = 0; index < 3; index += 1) {
    await octokit.rest.issues.create({ owner, repo, title: `graphql page fixture ${index}` });
  }
  const document = `query ($owner: String!, $name: String!, $cursor: String) {
      repository(owner: $owner, name: $name) {
        issues(first: 2, after: $cursor, states: [OPEN]) {
          totalCount
          pageInfo { hasNextPage endCursor }
          nodes { number }
        }
      }
    }`;
  const first = await octokit.graphql(document, { owner, name: repo, cursor: null });
  const connection = first?.repository?.issues;
  want(connection?.nodes?.length === 2, "2 nodes on the first page", connection?.nodes?.length,
    "first: 2 is ignored, so a client cannot control page size");
  want(connection?.pageInfo?.hasNextPage === true, "hasNextPage true", connection?.pageInfo?.hasNextPage,
    "the connection does not advertise a next page even though more issues exist");
  want(Boolean(connection?.pageInfo?.endCursor), "an endCursor", "empty",
    "the connection has no endCursor, so a client cannot ask for the next page");
  const second = await octokit.graphql(document, {
    owner, name: repo, cursor: connection.pageInfo.endCursor,
  });
  const nextNumbers = second?.repository?.issues?.nodes?.map((node) => node.number) ?? [];
  const firstNumbers = connection.nodes.map((node) => node.number);
  want(nextNumbers.length > 0, "results on the second page", nextNumbers.length,
    "following endCursor returned nothing, so cursor pagination does not advance");
  want(!nextNumbers.some((number) => firstNumbers.includes(number)),
    "a disjoint second page", `${firstNumbers} then ${nextNumbers}`,
    "the second page repeats the first, so a paginating client loops forever");
});

await check("graphql", "graphql.paginate walks a connection", "POST /api/graphql (paginated)", async () => {
  const result = await octokit.graphql.paginate(
    `query ($owner: String!, $name: String!, $cursor: String) {
       repository(owner: $owner, name: $name) {
         issues(first: 2, after: $cursor, states: [OPEN]) {
           totalCount
           pageInfo { hasNextPage endCursor }
           nodes { number title }
         }
       }
     }`,
    { owner, name: repo },
  );
  const nodes = result?.repository?.issues?.nodes ?? [];
  const total = result?.repository?.issues?.totalCount ?? 0;
  want(total > 2, "more issues than fit on one page", total, "the fixture did not produce enough issues to page over");
  want(nodes.length === total, `${total} nodes gathered across pages`, nodes.length,
    "octokit.graphql.paginate could not walk every page, which means pageInfo does not terminate correctly");
});

await check("graphql", "graphql rateLimit query", "POST /api/graphql", async () => {
  const result = await octokit.graphql(`query { rateLimit { limit remaining resetAt cost nodeCount } }`);
  want(typeof result?.rateLimit?.limit === "number", "a numeric limit", typeof result?.rateLimit?.limit,
    "the rateLimit field is missing, so a client cannot budget its query cost");
  want(typeof result?.rateLimit?.cost === "number", "a numeric cost", typeof result?.rateLimit?.cost,
    "the rateLimit field carries no cost");
  want(Boolean(result?.rateLimit?.resetAt), "a resetAt timestamp", "empty", "rateLimit has no resetAt");
});

await check("graphql", "graphql pull request document (the shape gh pr view sends)", "POST /api/graphql", async () => {
  // `gh` asks for a pull request and its review, check and merge state in one
  // document; a client that cannot read this cannot render `gh pr view`.
  const branch = "octokit-graphql-topic";
  const base = await octokit.rest.git.getRef({ owner, repo, ref: `heads/main` });
  await octokit.rest.git.createRef({ owner, repo, ref: `refs/heads/${branch}`, sha: base.data.object.sha });
  await octokit.rest.repos.createOrUpdateFileContents({
    owner, repo, path: "graphql-topic.txt", message: "graphql topic",
    content: Buffer.from("graphql\n").toString("base64"), branch,
  });
  const pull = await octokit.rest.pulls.create({
    owner, repo, title: "octokit graphql pull request", head: branch, base: "main",
  });
  const result = await octokit.graphql(
    `query ($owner: String!, $name: String!, $number: Int!) {
       repository(owner: $owner, name: $name) {
         pullRequest(number: $number) {
           id
           number
           title
           state
           isDraft
           mergeable
           baseRefName
           headRefName
           author { login }
           reviews(first: 5) { totalCount nodes { state } }
           commits(first: 5) { totalCount nodes { commit { oid } } }
           labels(first: 5) { nodes { name } }
           comments(first: 5) { totalCount }
         }
       }
     }`,
    { owner, name: repo, number: pull.data.number },
  );
  const node = result?.repository?.pullRequest;
  want(node?.number === pull.data.number, pull.data.number, node?.number, "the pull request node is wrong");
  want(node?.baseRefName === "main" && node?.headRefName === branch,
    `base main head ${branch}`, `${node?.baseRefName} / ${node?.headRefName}`,
    "the pull request node reports the wrong refs");
  want(node?.state === "OPEN", "OPEN", node?.state, "the pull request state enum is wrong");
  want(typeof node?.commits?.totalCount === "number", "a numeric commits.totalCount",
    typeof node?.commits?.totalCount, "the pull request has no commits connection");
  want(node?.author?.login === owner, owner, JSON.stringify(node?.author), "the pull request has no author");
  want(node?.isDraft === false, "isDraft false", node?.isDraft, "isDraft is not resolved");
});

await check("graphql", "graphql search connection", "POST /api/graphql", async () => {
  const result = await octokit.graphql(
    `query ($searchQuery: String!) {
       search(query: $searchQuery, type: REPOSITORY, first: 5) {
         repositoryCount
         pageInfo { hasNextPage }
         nodes { __typename ... on Repository { nameWithOwner } }
       }
     }`,
    { searchQuery: repo },
  );
  want(typeof result?.search?.repositoryCount === "number", "a numeric repositoryCount",
    typeof result?.search?.repositoryCount, "the search connection has no repositoryCount");
  want(Array.isArray(result?.search?.nodes), "a nodes array", typeof result?.search?.nodes,
    "the search connection returned no nodes array");
});

await check("graphql", "graphql introspection", "POST /api/graphql", async () => {
  const result = await octokit.graphql(
    `query { __schema { queryType { name } mutationType { name } } __type(name: "Repository") { name kind } }`,
  );
  want(result?.__schema?.queryType?.name === "Query", "Query", result?.__schema?.queryType?.name,
    "introspection does not name the query root, so code generators cannot run against this endpoint");
  want(result?.__schema?.mutationType?.name === "Mutation", "Mutation", result?.__schema?.mutationType?.name,
    "introspection does not name the mutation root");
  want(result?.__type?.kind === "OBJECT", "OBJECT", result?.__type?.kind,
    "__type(name: \"Repository\") does not resolve");
});

// --- Representational State Transfer surfaces octokit users lean on ----------
await check("repos", "repos.listCommits", "GET /repos/{owner}/{repo}/commits", async () => {
  const { data } = await octokit.rest.repos.listCommits({ owner, repo });
  want(data.length > 0, "at least one commit", data.length, "the commit listing is empty");
  want(Boolean(data[0].sha) && Boolean(data[0].commit?.message), "sha and commit.message", "absent",
    "a listed commit is missing the fields every client renders");
});

await check("repos", "repos.compareCommits", "GET /repos/{owner}/{repo}/compare/{basehead}", async () => {
  const { data } = await octokit.rest.repos.compareCommitsWithBasehead({
    owner, repo, basehead: `main...octokit-graphql-topic`,
  });
  want(typeof data.ahead_by === "number", "a numeric ahead_by", typeof data.ahead_by,
    "the comparison carries no ahead_by");
  want(Array.isArray(data.files), "a files array", typeof data.files, "the comparison carries no files array");
});

await check("checks", "checks.create and listForRef", "POST /repos/{owner}/{repo}/check-runs", async () => {
  const head = await octokit.rest.repos.listCommits({ owner, repo, per_page: 1 });
  const sha = head.data[0].sha;
  const created = await octokit.rest.checks.create({
    owner, repo, name: "octokit/conformance", head_sha: sha, status: "completed", conclusion: "success",
  });
  want(created.data.id > 0, "a check run id", created.data.id, "the created check run has no id");
  const listed = await octokit.rest.checks.listForRef({ owner, repo, ref: sha });
  want(listed.data.total_count >= 1, "at least one check run", listed.data.total_count,
    "the check run is not listed for its own commit");
});

await check("webhooks", "repos.createWebhook", "POST /repos/{owner}/{repo}/hooks", async () => {
  const { data } = await octokit.rest.repos.createWebhook({
    owner, repo,
    config: { url: "https://example.invalid/octokit", content_type: "json" },
    events: ["push"],
  });
  want(data.id > 0, "a webhook id", data.id, "the created webhook has no id");
  want(data.config.content_type === "json", "json", data.config.content_type,
    "content_type does not round trip");
  await octokit.rest.repos.deleteWebhook({ owner, repo, hook_id: data.id });
});

await check("deployments", "repos.createDeployment and status", "POST /repos/{owner}/{repo}/deployments", async () => {
  const created = await octokit.rest.repos.createDeployment({
    owner, repo, ref: "main", environment: "octokit", required_contexts: [], auto_merge: false,
  });
  want(typeof created.data.id === "number", "a numeric deployment id", typeof created.data.id,
    "the deployment was not created (octokit models the 202 accepted case as a different shape)");
  const status = await octokit.rest.repos.createDeploymentStatus({
    owner, repo, deployment_id: created.data.id, state: "success",
  });
  want(status.data.state === "success", "success", status.data.state, "the deployment status state is wrong");
});

await check("pagination", "octokit.paginate over commits", "GET /repos/{owner}/{repo}/commits?per_page=1", async () => {
  const all = await octokit.paginate(octokit.rest.repos.listCommits, { owner, repo, per_page: 1 });
  want(all.length >= 2, "every commit across pages", all.length,
    "octokit.paginate stopped early over commits, which means the Link header is wrong on that collection");
});

await check("pagination", "per_page is clamped at 100", "GET /repos/{owner}/{repo}/issues?per_page=200", async () => {
  const response = await octokit.request("GET /repos/{owner}/{repo}/issues", {
    owner, repo, state: "all", per_page: 200,
  });
  want(response.status === 200, "200", response.status, "an oversized per_page was refused rather than clamped");
  want(response.data.length <= 100, "at most 100 results", response.data.length,
    "per_page is not clamped to the documented maximum of 100");
});

await check("transport", "404 error body carries documentation_url", "GET /repos/{owner}/nope", async () => {
  try {
    await octokit.request("GET /repos/{owner}/{repo}", { owner, repo: "nope-not-here" });
  } catch (error) {
    want(error.status === 404, "404", error.status, "a missing repository does not answer 404");
    want(Boolean(error.response?.data?.documentation_url), "documentation_url in the error body", "absent",
      "the error body omits documentation_url, which octokit surfaces to the user");
    return;
  }
  throw new Deviation("a 404", "success", "a missing repository was served");
});

await check("transport", "unauthenticated request is 401 with a WWW-Authenticate-shaped body",
  "GET /user with no credential", async () => {
    const anonymous = new Octokit({ baseUrl: `${base}/api/v3` });
    try {
      await anonymous.rest.users.getAuthenticated();
    } catch (error) {
      want(error.status === 401, "401", error.status, "an unauthenticated /user did not answer 401");
      want(Boolean(error.response?.data?.message), "a message in the error body", "absent",
        "the 401 body carries no message");
      return;
    }
    throw new Deviation("a 401", "success", "an unauthenticated request to /user succeeded");
  });

// --- OAuth device grant, driven by octokit's own strategy --------------------
// This is how `gh auth login` and every headless client obtains a credential.
// A GitHub App is provisioned through the manifest flow to supply a client id,
// and the browser half of the grant is performed the way a person would: sign
// in to the web session, then submit the user code the client printed.
await check("transport", "OAuth device flow", "POST /login/device/code", async () => {
  const { createOAuthDeviceAuth } = await import(
    join(repoRoot, "web", "node_modules", "@octokit", "auth-oauth-device", "dist-bundle", "index.js")
  );

  const manifest = JSON.stringify({
    name: "Octokit Conformance Device App",
    url: "https://example.invalid/app",
    redirect_url: "https://example.invalid/callback",
    default_permissions: { contents: "read" },
  });
  const formPost = (path, body, cookie) =>
    fetch(`${base}${path}`, {
      method: "POST",
      redirect: "manual",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/x-www-form-urlencoded",
        ...(cookie ? { Cookie: cookie } : {}),
      },
      body: new URLSearchParams(body).toString(),
    });

  const started = await formPost("/settings/apps/new", { manifest });
  const location = started.headers.get("location") ?? "";
  want(Boolean(location), "a redirect carrying a conversion code", started.status,
    "the App manifest form did not redirect");
  const conversionCode = new URL(location, base).searchParams.get("code");
  want(Boolean(conversionCode), "a conversion code", location, "the redirect carried no code");
  const conversion = await fetch(`${base}/api/v3/app-manifests/${conversionCode}/conversions`, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}` },
  });
  const app = await conversion.json();
  want(Boolean(app.client_id), "a client_id on the converted App", JSON.stringify(app),
    "the manifest conversion returned no client_id, so no device grant can be started");

  // A browser session, which is what the approval form requires.
  const signIn = await formPost("/login", { login: owner, password: token });
  const sessionCookie = (signIn.headers.getSetCookie?.() ?? [])
    .map((value) => value.split(";")[0])
    .join("; ");
  want(Boolean(sessionCookie), "a session cookie from the web sign-in", signIn.status,
    "signing in through the web form set no session cookie, so the grant cannot be approved");

  let verified = null;
  const auth = createOAuthDeviceAuth({
    clientType: "github-app",
    clientId: app.client_id,
    request: octokit.request.defaults({ baseUrl: base }),
    onVerification: async (verification) => {
      verified = verification;
      // Approve the code the same way the person reading it would.
      await formPost("/login/device", { user_code: verification.user_code }, sessionCookie);
    },
  });

  const authentication = await auth({ type: "oauth" });
  want(Boolean(verified?.user_code), "a user code the client can print", JSON.stringify(verified),
    "the device-code response carried no user_code");
  want(Boolean(verified?.verification_uri), "a verification_uri", JSON.stringify(verified),
    "the device-code response carried no verification_uri");
  want(Boolean(authentication?.token), "an access token", JSON.stringify(authentication),
    "the device grant produced no token");

  const granted = new Octokit({ auth: authentication.token, baseUrl: `${base}/api/v3` });
  const { data } = await granted.rest.users.getAuthenticated();
  want(data.login === owner, owner, data.login,
    "the token from the device grant authenticates as the wrong account");
});

console.error(`octokit driver: ${passed} passed, ${failed} failed, ${skipped} skipped`);
