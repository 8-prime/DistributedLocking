# Distributed Locking Benchmark

A benchmark suite for comparing distributed locking service implementations across different languages and frameworks. Each solution exposes the same HTTP API (see [spec.md](spec.md)) and is run in an isolated Docker container. The benchmark runner measures throughput (RPS) and latency (p50/p95/p99) across several concurrency scenarios.

## How it works

Solutions live under `solutions/`. Each one needs a `Dockerfile`, a `solution.json` manifest, and an HTTP server implementing the [lock API](spec.md).

The benchmark runner (`benchmark/`) builds each solution's Docker image, starts it, waits for the health check, then runs a series of scenarios:

| Scenario         | Workers                | Keys   | Notes                                     |
| ---------------- | ---------------------- | ------ | ----------------------------------------- |
| sequential       | 1                      | 1 000  | Baseline single-threaded throughput       |
| low_concurrency  | 10                     | 1 000  | Light parallel load                       |
| high_concurrency | 100                    | 10 000 | Sustained parallel load, partitioned keys |
| contention       | 50                     | 5      | Heavy lock contention on shared keys      |
| list_heavy       | 20 writers + 5 readers | 1 000  | Mixed write + `GET /locks` read load      |

Each scenario runs a 5 s warmup followed by a 15 s measurement window.

**Running locally** (requires Go 1.24+ and Docker):

```bash
cd benchmark
go run . ../solutions               # all solutions
go run . ../solutions/go-inmemory   # single solution
go run . --duration 30s --warmup 10s ../solutions
```

**Running via Docker** (no Go install needed on the host):

First build the image:

```bash
docker build -t dl-benchmark-runner .
```

Then run. The Docker socket must be mounted so the runner can build and start solution containers on the host daemon. The socket path differs by OS:

_Linux / macOS:_
```bash
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$(pwd)/results:/results" \
  dl-benchmark-runner
```

_Windows (PowerShell) — Docker Desktop exposes a named pipe instead of a socket. Use forward slashes to avoid PowerShell escaping issues:_
```powershell
docker run --rm `
  -v //./pipe/docker_engine://./pipe/docker_engine `
  -v "${PWD}\results:/results" `
  dl-benchmark-runner
```

If that fails with a pipe-not-found error, check the actual pipe name Docker Desktop is using on your machine:
```powershell
Get-ChildItem //./pipe/ | Where-Object { $_.Name -like "*docker*" }
```
Common alternatives are `dockerDesktopLinuxEngine` or `dockerDesktopEngine`. Substitute whichever appears on both sides of the `-v` mount.

To override duration or warmup, append flags after the image name:

```bash
dl-benchmark-runner --duration 30s --warmup 10s solutions/
```

The runner detects it is inside Docker and automatically creates an isolated bridge network for the run. Solution containers are started as siblings on the host daemon (Docker-out-of-Docker via the socket mount) and communicate with the runner over that network. The network is removed when the run completes.

Results are written to `results/` as both JSON and Markdown.

## Results

> Measured on **Intel Core i5-13600KF**, **DDR4 3600 MHz CL16**

Summary (RPS / p99 ms):

| Scenario         | Go In-Memory    | Zig In-Memory   | Zig ZAP In-Memory |
| ---------------- | --------------- | --------------- | ----------------- |
| sequential       | 1 132 / 2.2 ms  | 486 / 4.2 ms    | 2 670 / 1.1 ms    |
| low_concurrency  | 2 296 / 19.2 ms | 674 / 51.0 ms   | 18 919 / 1.1 ms   |
| high_concurrency | 197 / 7.8 ms    | 776 / 1323.3 ms | 49 954 / 4.7 ms   |
| contention       | 728 / 821.7 ms  | 488 / 822.1 ms  | 43 145 / 2.7 ms   |
| list_heavy       | 964 / 613.2 ms  | —               | 34 628 / 1.6 ms   |

The Zig ZAP solution leads across all scenarios by a wide margin. The Go in-memory solution degrades sharply under high concurrency (nearly all requests error out at 100 workers). The Zig in-memory solution didn't complete the `list_heavy` scenario and shows significant lock leakage, suggesting correctness issues under load.
