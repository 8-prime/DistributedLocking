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

To override duration or warmup, append flags after the image name:

```bash
dl-benchmark-runner --duration 30s --warmup 10s solutions/
```

The runner detects it is inside Docker and automatically creates an isolated bridge network for the run. Solution containers are started as siblings on the host daemon (Docker-out-of-Docker via the socket mount) and communicate with the runner over that network. The network is removed when the run completes.

Results are written to `results/` as both JSON and Markdown.

## Results

> Measured on **Intel Core i5-13600KF**, **DDR4 3600 MHz CL16**

Summary (RPS / p99 ms):

| Scenario         | Bun In-Memory     | C epoll In-Memory  | Dotnet Eventloop   | Dotnet In-Memory   | Go In-Memory      | Zig In-Memory     | Zig ZAP In-Memory  |
| ---------------- | ----------------- | ------------------ | ------------------ | ------------------ | ----------------- | ----------------- | ------------------ |
| sequential       | 13035 rps / 0.2ms | 15447 rps / 0.1ms  | 7523 rps / 0.3ms   | 7929 rps / 0.2ms   | 5573 rps / 0.3ms  | 14129 rps / 0.2ms | 13422 rps / 0.1ms  |
| low_concurrency  | 74870 rps / 0.7ms | 75686 rps / 0.6ms  | 50178 rps / 0.9ms  | 49699 rps / 0.8ms  | 24906 rps / 1.0ms | —                 | 57928 rps / 0.6ms  |
| high_concurrency | 87800 rps / 2.9ms | 264050 rps / 2.8ms | 160400 rps / 2.7ms | 173973 rps / 2.7ms | 73248 rps / 4.8ms | —                 | 213985 rps / 2.5ms |
| contention       | 93542 rps / 1.7ms | 265596 rps / 1.5ms | 120123 rps / 1.8ms | 124276 rps / 1.8ms | 73416 rps / 2.5ms | —                 | 169372 rps / 1.7ms |
| list_heavy       | 78136 rps / 1.2ms | 148854 rps / 0.9ms | 81312 rps / 1.3ms  | 87101 rps / 1.3ms  | 45823 rps / 1.7ms | —                 | 111246 rps / 1.2ms |

