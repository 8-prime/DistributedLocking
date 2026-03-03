# Distributed Locking API Specification

## Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/lock` | POST | Acquire a lock |
| `/lock` | DELETE | Release a lock |
| `/locks` | GET | List all held locks |
| `/healthz` | GET | Health check (always 200 OK when ready) |

---

## POST /lock

Acquire a lock on a key.

**Request body:**
```json
{ "key": "string", "lockee": "string", "force": false }
```

- `key`: The resource identifier to lock
- `lockee`: Identifier of who is acquiring the lock
- `force` (optional): If true, forcibly acquire even if held by another lockee

**Responses:**

- `200 OK` — Lock acquired
  ```json
  { "locked": true, "key": "...", "lockee": "..." }
  ```

- `409 Conflict` — Lock already held by another lockee
  ```json
  { "locked": false, "key": "...", "currentLockee": "..." }
  ```

---

## DELETE /lock

Release a lock.

**Request body:**
```json
{ "key": "string", "lockee": "string" }
```

**Responses:**

- `200 OK` — Lock released
- `403 Forbidden` — `lockee` does not hold this lock
- `404 Not Found` — No lock exists for this key

---

## GET /locks

List all currently held locks.

**Response:**
```json
{
  "locks": [
    { "key": "string", "lockee": "string", "since": "2024-01-01T00:00:00Z" }
  ]
}
```

---

## GET /healthz

Health/readiness check. Returns `200 OK` when the service is ready to handle requests.

**Response:** `200 OK` with empty body or `{ "status": "ok" }`

---

## Notes

- All request/response bodies are JSON (`Content-Type: application/json`)
- The `since` field in lock listings is an RFC 3339 timestamp
- Implementations must be stateless at the HTTP level (no sessions/cookies required)
