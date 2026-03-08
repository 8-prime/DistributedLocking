interface LockEntry {
  key: string;
  lockee: string;
  since: string;
}

const locks = new Map<string, LockEntry>();

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

Bun.serve({
  port: 8080,
  async fetch(req) {
    const url = new URL(req.url);
    const path = url.pathname.replace(/\/+$/, "") || "/";

    if (path === "/healthz") {
      return json(200, { status: "ok" });
    }

    if (path === "/lock" && req.method === "POST") {
      let body: { key?: string; lockee?: string; force?: boolean };
      try {
        body = await req.json();
      } catch {
        return new Response("bad request", { status: 400 });
      }
      const { key, lockee, force = false } = body;
      if (!key || !lockee) {
        return new Response("key and lockee are required", { status: 400 });
      }

      const existing = locks.get(key);
      if (existing) {
        if (existing.lockee === lockee) {
          // Idempotent re-acquire: preserve original since.
          return json(200, { locked: true, key, lockee });
        }
        if (!force) {
          return json(409, { locked: false, key, currentLockee: existing.lockee });
        }
      }
      locks.set(key, { key, lockee, since: new Date().toISOString() });
      return json(200, { locked: true, key, lockee });
    }

    if (path === "/unlock" && req.method === "POST") {
      let body: { key?: string; lockee?: string };
      try {
        body = await req.json();
      } catch {
        return new Response("bad request", { status: 400 });
      }
      const { key, lockee } = body;
      if (!key || !lockee) {
        return new Response("key and lockee are required", { status: 400 });
      }

      const existing = locks.get(key);
      if (!existing) {
        return new Response("not found", { status: 404 });
      }
      if (existing.lockee !== lockee) {
        return new Response("lockee mismatch", { status: 403 });
      }
      locks.delete(key);
      return new Response(null, { status: 200 });
    }

    if (path === "/locks" && req.method === "GET") {
      return json(200, { locks: Array.from(locks.values()) });
    }

    return new Response("not found", { status: 404 });
  },
});
