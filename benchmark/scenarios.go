package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type scenarioCfg struct {
	ID            string
	Concurrency   int
	KeyPoolSize   int
	Duration      time.Duration
	Warmup        time.Duration
	PartitionKeys bool // if true, each worker gets a unique slice of the key space
}

var defaultScenarios = []scenarioCfg{
	{ID: "sequential", Concurrency: 1, KeyPoolSize: 1000, PartitionKeys: true},
	{ID: "low_concurrency", Concurrency: 10, KeyPoolSize: 1000, PartitionKeys: true},
	{ID: "high_concurrency", Concurrency: 100, KeyPoolSize: 10000, PartitionKeys: true},
	{ID: "contention", Concurrency: 50, KeyPoolSize: 5, PartitionKeys: false},
	// list_heavy handled specially
}

type worker struct {
	id      int
	baseURL string
	client  *http.Client
	keys    []string
	lockee  string

	latencies chan int64
	errors    *atomic.Int64
	conflicts *atomic.Int64
	total     *atomic.Int64
}

func (w *worker) acquireRelease(ctx context.Context) {
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		key := w.keys[i%len(w.keys)]

		// Acquire
		start := time.Now()
		statusCode, err := w.postLock(key, w.lockee, false)
		elapsed := time.Since(start).Nanoseconds()

		w.total.Add(1)
		if err != nil {
			w.errors.Add(1)
			continue
		}
		w.latencies <- elapsed

		switch statusCode {
		case http.StatusConflict:
			w.conflicts.Add(1)
			// Don't try to release a lock we didn't acquire.
			continue
		case http.StatusOK:
			// good
		default:
			w.errors.Add(1)
			continue
		}

		// Release
		start = time.Now()
		releaseCode, err := w.deleteLock(key, w.lockee)
		elapsed = time.Since(start).Nanoseconds()

		w.total.Add(1)
		if err != nil || (releaseCode != http.StatusOK && releaseCode != http.StatusNotFound) {
			w.errors.Add(1)
			continue
		}
		w.latencies <- elapsed
	}
}

func (w *worker) postLock(key, lockee string, force bool) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"key": key, "lockee": lockee, "force": force,
	})
	req, _ := http.NewRequest(http.MethodPost, w.baseURL+"/lock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (w *worker) deleteLock(key, lockee string) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"key": key, "lockee": lockee,
	})
	req, _ := http.NewRequest(http.MethodDelete, w.baseURL+"/lock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func runScenario(baseURL string, cfg scenarioCfg) ScenarioResult {
	transport := &http.Transport{
		MaxIdleConnsPerHost: cfg.Concurrency * 2,
	}
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	latencies := make(chan int64, 1<<20)
	var errCount, conflictCount, totalCount atomic.Int64

	buildWorker := func(id int) *worker {
		var keys []string
		if cfg.PartitionKeys {
			perWorker := cfg.KeyPoolSize / cfg.Concurrency
			if perWorker < 1 {
				perWorker = 1
			}
			offset := id * perWorker
			keys = make([]string, perWorker)
			for j := range keys {
				keys[j] = fmt.Sprintf("key-%d", offset+j)
			}
		} else {
			// Contention: all workers share the same key pool
			keys = make([]string, cfg.KeyPoolSize)
			for j := range keys {
				keys[j] = fmt.Sprintf("key-%d", j)
			}
		}
		return &worker{
			id:        id,
			baseURL:   baseURL,
			client:    httpClient,
			keys:      keys,
			lockee:    fmt.Sprintf("worker-%d", id),
			latencies: latencies,
			errors:    &errCount,
			conflicts: &conflictCount,
			total:     &totalCount,
		}
	}

	runPhase := func(dur time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), dur)
		defer cancel()

		var wg sync.WaitGroup
		for i := 0; i < cfg.Concurrency; i++ {
			wg.Add(1)
			w := buildWorker(i)
			go func() {
				defer wg.Done()
				w.acquireRelease(ctx)
			}()
		}
		wg.Wait()
	}

	// Warmup — discard metrics
	if cfg.Warmup > 0 {
		// Reset counters, run warmup, then reset again
		runPhase(cfg.Warmup)
		errCount.Store(0)
		conflictCount.Store(0)
		totalCount.Store(0)
		// Drain latency channel
		for len(latencies) > 0 {
			<-latencies
		}
	}

	// Measurement
	start := time.Now()
	runPhase(cfg.Duration)
	elapsed := time.Since(start)

	close(latencies)

	samples := make([]int64, 0, len(latencies))
	for ns := range latencies {
		samples = append(samples, ns)
	}

	total := totalCount.Load()
	errors := errCount.Load()
	conflicts := conflictCount.Load()
	durationMs := elapsed.Milliseconds()

	var rps float64
	if elapsed.Seconds() > 0 {
		rps = float64(total) / elapsed.Seconds()
	}

	var errorRate, conflictRate float64
	if total > 0 {
		errorRate = float64(errors) / float64(total)
		conflictRate = float64(conflicts) / float64(total)
	}

	leakedLocks := checkLeakedLocks(baseURL)
	return ScenarioResult{
		ScenarioID:    cfg.ID,
		RequestsTotal: total,
		DurationMs:    durationMs,
		RPS:           rps,
		P50Ms:         percentileMs(samples, 50),
		P95Ms:         percentileMs(samples, 95),
		P99Ms:         percentileMs(samples, 99),
		MaxMs:         maxMs(samples),
		ErrorRate:     errorRate,
		ConflictRate:  conflictRate,
		LeakedLocks:   leakedLocks,
	}
}

func runListHeavy(baseURL string, warmup, duration time.Duration) ScenarioResult {
	const writers = 20
	const readers = 5
	const keyPool = 1000

	transport := &http.Transport{MaxIdleConnsPerHost: (writers + readers) * 2}
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	latencies := make(chan int64, 1<<20)
	var errCount, conflictCount, totalCount atomic.Int64

	runPhase := func(dur time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), dur)
		defer cancel()
		var wg sync.WaitGroup

		// Writers: acquire+release
		for i := 0; i < writers; i++ {
			wg.Add(1)
			id := i
			go func() {
				defer wg.Done()
				w := &worker{
					id:        id,
					baseURL:   baseURL,
					client:    httpClient,
					keys:      makeKeys(id, keyPool, writers),
					lockee:    fmt.Sprintf("writer-%d", id),
					latencies: latencies,
					errors:    &errCount,
					conflicts: &conflictCount,
					total:     &totalCount,
				}
				w.acquireRelease(ctx)
			}()
		}

		// Readers: poll GET /locks
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					default:
					}
					start := time.Now()
					resp, err := httpClient.Get(baseURL + "/locks")
					elapsed := time.Since(start).Nanoseconds()
					totalCount.Add(1)
					if err != nil {
						errCount.Add(1)
						continue
					}
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						errCount.Add(1)
						continue
					}
					latencies <- elapsed
				}
			}()
		}

		wg.Wait()
	}

	// Warmup
	if warmup > 0 {
		runPhase(warmup)
		errCount.Store(0)
		conflictCount.Store(0)
		totalCount.Store(0)
		for len(latencies) > 0 {
			<-latencies
		}
	}

	start := time.Now()
	runPhase(duration)
	elapsed := time.Since(start)
	close(latencies)

	samples := make([]int64, 0, len(latencies))
	for ns := range latencies {
		samples = append(samples, ns)
	}

	total := totalCount.Load()
	errors := errCount.Load()
	conflicts := conflictCount.Load()
	durationMs := elapsed.Milliseconds()
	var rps float64
	if elapsed.Seconds() > 0 {
		rps = float64(total) / elapsed.Seconds()
	}
	var errorRate, conflictRate float64
	if total > 0 {
		errorRate = float64(errors) / float64(total)
		conflictRate = float64(conflicts) / float64(total)
	}

	leakedLocks := checkLeakedLocks(baseURL)
	return ScenarioResult{
		ScenarioID:    "list_heavy",
		RequestsTotal: total,
		DurationMs:    durationMs,
		RPS:           rps,
		P50Ms:         percentileMs(samples, 50),
		P95Ms:         percentileMs(samples, 95),
		P99Ms:         percentileMs(samples, 99),
		MaxMs:         maxMs(samples),
		ErrorRate:     errorRate,
		ConflictRate:  conflictRate,
		LeakedLocks:   leakedLocks,
	}
}

func checkLeakedLocks(baseURL string) int {
	resp, err := http.Get(baseURL + "/locks")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var payload struct {
		Locks []json.RawMessage `json:"locks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return -1
	}
	return len(payload.Locks)
}

func makeKeys(workerID, poolSize, totalWorkers int) []string {
	perWorker := poolSize / totalWorkers
	if perWorker < 1 {
		perWorker = 1
	}
	offset := workerID * perWorker
	keys := make([]string, perWorker)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", offset+i)
	}
	return keys
}

func percentileMs(samples []int64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return float64(sorted[idx]) / 1e6
}

func maxMs(samples []int64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var m int64
	for _, v := range samples {
		if v > m {
			m = v
		}
	}
	return float64(m) / 1e6
}
