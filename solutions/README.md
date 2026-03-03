# Adding a New Solution

Each solution is a self-contained directory under `solutions/` that the benchmark runner auto-discovers.

## Steps

1. **Create a directory** under `solutions/` with a descriptive name (e.g. `solutions/my-redis-solution/`).

2. **Add `solution.json`** — describes the solution to the runner:
   ```json
   {
     "name": "My Redis Solution",
     "description": "Brief description of the implementation",
     "port": 8080,
     "startupTimeoutMs": 10000
   }
   ```
   - `name`: Human-readable name (appears in reports)
   - `description`: One-line description
   - `port`: The port your service listens on **inside** the container
   - `startupTimeoutMs`: How long the runner waits for `/healthz` to return 200

3. **Add a `Dockerfile`** that builds and runs your service. The final image must:
   - Start an HTTP server on the declared `port`
   - Expose that port

4. **Implement the API** per `spec.md`:
   - `POST /lock` — acquire a lock
   - `DELETE /lock` — release a lock
   - `GET /locks` — list all held locks
   - `GET /healthz` — return 200 when ready

## Running Your Solution

```bash
# Benchmark only your solution
cd benchmark
go run . ../solutions/my-redis-solution

# Benchmark all solutions
go run . ../solutions

# Custom durations
go run . --duration 30s --warmup 10s ../solutions
```

Results are written to `results/benchmark-<timestamp>.json` and `.md`.

## Tips

- The runner uses Docker — no host runtime needed (Go, Node, Python, etc.)
- `/healthz` is polled with exponential backoff up to `startupTimeoutMs`
- The benchmark assigns a random host port; your container always sees its declared `port`
- Correctness matters: the `contention` scenario intentionally creates lock contention — your 409 responses should be well-formed
