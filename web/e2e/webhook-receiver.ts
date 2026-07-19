import { createHmac, timingSafeEqual } from "node:crypto";

const port = Number(process.env.BLEEPHUB_E2E_WEBHOOK_PORT ?? "15557");
const secret = process.env.BLEEPHUB_E2E_WEBHOOK_SECRET;
if (!secret) {
  throw new Error("BLEEPHUB_E2E_WEBHOOK_SECRET is required");
}

const events: Array<{ event: string; body: unknown }> = [];

Bun.serve({
  hostname: "127.0.0.1",
  port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/health") {
      return Response.json({ status: "ok" });
    }
    if (request.method === "GET" && url.pathname === "/events") {
      return Response.json(events);
    }
    if (request.method !== "POST" || url.pathname !== "/marketplace") {
      return new Response("not found", { status: 404 });
    }

    const body = Buffer.from(await request.arrayBuffer());
    const actual = request.headers.get("x-hub-signature-256") ?? "";
    const expected = `sha256=${createHmac("sha256", secret).update(body).digest("hex")}`;
    const actualBytes = Buffer.from(actual);
    const expectedBytes = Buffer.from(expected);
    if (actualBytes.length !== expectedBytes.length || !timingSafeEqual(actualBytes, expectedBytes)) {
      return new Response("invalid signature", { status: 401 });
    }
    events.push({
      event: request.headers.get("x-github-event") ?? "",
      body: JSON.parse(body.toString("utf8")),
    });
    return new Response(null, { status: 204 });
  },
});
