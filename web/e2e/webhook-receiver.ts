import { createHmac, timingSafeEqual } from "node:crypto";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

function requireEnv(name: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

const port = Number(process.env.BLEEPHUB_E2E_WEBHOOK_PORT ?? "15557");
const secret = requireEnv("BLEEPHUB_E2E_WEBHOOK_SECRET");

const events: Array<{ event: string; body: unknown }> = [];

function sendJSON(response: ServerResponse, status: number, payload: unknown) {
  response.writeHead(status, { "content-type": "application/json" });
  response.end(JSON.stringify(payload));
}

function readBody(request: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    request.on("data", (chunk: Buffer) => chunks.push(chunk));
    request.on("end", () => resolve(Buffer.concat(chunks)));
    request.on("error", reject);
  });
}

function headerValue(raw: string | string[] | undefined): string {
  return typeof raw === "string" ? raw : "";
}

async function handle(request: IncomingMessage, response: ServerResponse) {
  const pathname = new URL(request.url ?? "/", `http://127.0.0.1:${port}`).pathname;

  if (request.method === "GET" && pathname === "/health") {
    sendJSON(response, 200, { status: "ok" });
    return;
  }
  if (request.method === "GET" && pathname === "/events") {
    sendJSON(response, 200, events);
    return;
  }
  if (request.method !== "POST" || pathname !== "/marketplace") {
    response.writeHead(404).end("not found");
    return;
  }

  const body = await readBody(request);
  const expected = `sha256=${createHmac("sha256", secret).update(body).digest("hex")}`;
  const actualBytes = Buffer.from(headerValue(request.headers["x-hub-signature-256"]));
  const expectedBytes = Buffer.from(expected);
  if (actualBytes.length !== expectedBytes.length || !timingSafeEqual(actualBytes, expectedBytes)) {
    response.writeHead(401).end("invalid signature");
    return;
  }

  events.push({
    event: headerValue(request.headers["x-github-event"]),
    body: JSON.parse(body.toString("utf8")),
  });
  response.writeHead(204).end();
}

createServer((request, response) => {
  handle(request, response).catch((err: unknown) => {
    response.writeHead(500).end(err instanceof Error ? err.message : "receiver failure");
  });
}).listen(port, "127.0.0.1");
