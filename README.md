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

> Measured on **Intel Core i7-6700**, **DDR4 2133 MHz**

# Benchmark Report

**Timestamp:** 2026-05-03T21:24:55Z

## Results (RPS / P99 ms)

| Scenario | Bun In-Memory | C epoll In-Memory | Dotnet Eventloop | Dotnet In-Memory | Go In-Memory | Go Sharded | Zig In-Memory | Zig ZAP In-Memory |
|---|---|---|---|---|---|---|---|---|
| sequential | 11637 RPS / 0.2 ms | 15112 RPS / 0.1 ms | 8141 RPS / 0.2 ms | 8907 RPS / 0.2 ms | 11081 RPS / 0.1 ms | 10455 RPS / 0.2 ms | 13777 RPS / 0.1 ms | 10649 RPS / 0.2 ms |
| low_concurrency | 28855 RPS / 1.0 ms | 77104 RPS / 0.5 ms | 33469 RPS / 1.4 ms | 41214 RPS / 1.2 ms | 51063 RPS / 0.6 ms | 51036 RPS / 0.6 ms | — | 60710 RPS / 0.5 ms |
| high_concurrency | 28398 RPS / 6.5 ms | 93375 RPS / 5.3 ms | 56376 RPS / 5.6 ms | 49968 RPS / 6.7 ms | 60564 RPS / 7.4 ms | 64699 RPS / 6.9 ms | — | 81158 RPS / 4.8 ms |
| contention | 28454 RPS / 3.6 ms | 78113 RPS / 3.8 ms | 48443 RPS / 3.4 ms | 51590 RPS / 3.2 ms | 61602 RPS / 3.7 ms | 54252 RPS / 4.4 ms | — | 75290 RPS / 2.8 ms |
| list_heavy | 26594 RPS / 2.0 ms | 77180 RPS / 1.7 ms | 34727 RPS / 2.6 ms | 42464 RPS / 1.9 ms | 44091 RPS / 2.3 ms | 54832 RPS / 2.3 ms | — | 39307 RPS / 0.0 ms |

## Detailed Metrics

### Bun In-Memory

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 174555 | 11637 | 0.1 | 0.1 | 0.2 | 4.1 | 0.00% | 0.00% | 1 |
| low_concurrency | 432830 | 28855 | 0.3 | 0.6 | 1.0 | 4.7 | 0.00% | 0.05% | 6 |
| high_concurrency | 426010 | 28398 | 3.5 | 4.4 | 6.5 | 12.2 | 0.02% | 0.00% | 49 |
| contention | 426818 | 28454 | 1.7 | 2.5 | 3.6 | 8.8 | 0.01% | 82.06% | 53 |
| list_heavy | 398921 | 26594 | 0.9 | 1.7 | 2.0 | 6.1 | 0.01% | 0.51% | 62 |

### C epoll In-Memory

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 226684 | 15112 | 0.1 | 0.1 | 0.1 | 3.7 | 0.00% | 0.00% | 1 |
| low_concurrency | 1156578 | 77104 | 0.1 | 0.3 | 0.5 | 4.2 | 0.00% | 0.05% | 3 |
| high_concurrency | 1400699 | 93375 | 0.7 | 3.5 | 5.3 | 15.9 | 0.01% | 0.00% | 47 |
| contention | 1171758 | 78113 | 0.4 | 2.2 | 3.8 | 16.8 | 0.00% | 81.99% | 51 |
| list_heavy | 1157726 | 77180 | 0.2 | 0.9 | 1.7 | 7.5 | 0.00% | 0.43% | 63 |

### Dotnet Eventloop

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 122114 | 8141 | 0.1 | 0.2 | 0.2 | 2.3 | 0.00% | 0.00% | 1 |
| low_concurrency | 502083 | 33469 | 0.2 | 0.7 | 1.4 | 14.5 | 0.00% | 0.00% | 5 |
| high_concurrency | 845682 | 56376 | 1.6 | 3.7 | 5.6 | 17.6 | 0.01% | 0.00% | 53 |
| contention | 726653 | 48443 | 0.9 | 2.1 | 3.4 | 12.2 | 0.00% | 81.97% | 58 |
| list_heavy | 520918 | 34727 | 0.6 | 1.5 | 2.6 | 21.5 | 0.00% | 0.38% | 66 |

### Dotnet In-Memory

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 133611 | 8907 | 0.1 | 0.2 | 0.2 | 3.6 | 0.00% | 0.00% | 1 |
| low_concurrency | 618219 | 41214 | 0.2 | 0.5 | 1.2 | 13.6 | 0.00% | 0.05% | 4 |
| high_concurrency | 749547 | 49968 | 1.7 | 4.4 | 6.7 | 24.5 | 0.01% | 0.01% | 53 |
| contention | 773877 | 51590 | 0.9 | 2.0 | 3.2 | 10.8 | 0.00% | 82.02% | 57 |
| list_heavy | 636973 | 42464 | 0.5 | 1.1 | 1.9 | 13.5 | 0.00% | 0.49% | 69 |

### Go In-Memory

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 166217 | 11081 | 0.1 | 0.1 | 0.1 | 3.4 | 0.00% | 0.00% | 1 |
| low_concurrency | 765957 | 51063 | 0.2 | 0.4 | 0.6 | 4.6 | 0.00% | 0.05% | 8 |
| high_concurrency | 908581 | 60564 | 1.1 | 4.9 | 7.4 | 23.6 | 0.01% | 0.01% | 53 |
| contention | 924081 | 61602 | 0.6 | 2.4 | 3.7 | 12.9 | 0.00% | 81.92% | 57 |
| list_heavy | 661416 | 44091 | 0.4 | 1.5 | 2.3 | 8.5 | 0.00% | 0.37% | 65 |

### Go Sharded

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 156819 | 10455 | 0.1 | 0.1 | 0.2 | 3.6 | 0.00% | 0.00% | 1 |
| low_concurrency | 765546 | 51036 | 0.2 | 0.4 | 0.6 | 5.4 | 0.00% | 0.05% | 5 |
| high_concurrency | 970536 | 64699 | 1.0 | 4.6 | 6.9 | 19.8 | 0.01% | 0.01% | 43 |
| contention | 813820 | 54252 | 0.6 | 2.8 | 4.4 | 15.1 | 0.00% | 81.93% | 48 |
| list_heavy | 822490 | 54832 | 0.3 | 1.2 | 2.3 | 8.2 | 0.00% | 0.55% | 58 |

### Zig In-Memory

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 206655 | 13777 | 0.1 | 0.1 | 0.1 | 2.2 | 0.00% | 0.00% | 0 |

### Zig ZAP In-Memory

| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% | Leaked |
|---|---|---|---|---|---|---|---|---|---|
| sequential | 159735 | 10649 | 0.1 | 0.1 | 0.2 | 3.7 | 0.00% | 0.00% | 0 |
| low_concurrency | 910673 | 60710 | 0.1 | 0.3 | 0.5 | 4.9 | 0.00% | 0.00% | 6 |
| high_concurrency | 1217442 | 81158 | 0.9 | 3.2 | 4.8 | 20.6 | 0.01% | 0.00% | 51 |
| contention | 1129425 | 75290 | 0.5 | 1.7 | 2.8 | 11.1 | 0.00% | 81.95% | 56 |
| list_heavy | 589608 | 39307 | 0.0 | 0.0 | 0.0 | 0.0 | 100.00% | 0.00% | -1 |
