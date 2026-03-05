package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/client"
)

func discoverSolutions(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}

	// If root itself has solution.json, it's a single solution directory.
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(root, "solution.json")); err == nil {
			return []string{root}, nil
		}
	}

	// Otherwise walk one level deep looking for solution.json.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(candidate, "solution.json")); err == nil {
			dirs = append(dirs, candidate)
		}
	}
	return dirs, nil
}

func benchmarkSolution(ctx context.Context, cli *client.Client, dir string, warmup, duration time.Duration) (SolutionResult, error) {
	// Load manifest.
	manifestPath := filepath.Join(dir, "solution.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return SolutionResult{}, fmt.Errorf("read solution.json: %w", err)
	}
	var manifest SolutionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return SolutionResult{}, fmt.Errorf("parse solution.json: %w", err)
	}

	tag := ImageTag(manifest.Name)
	fmt.Printf("\n=== %s ===\n", manifest.Name)
	fmt.Printf("Building image %s ...\n", tag)

	if err := BuildImage(ctx, cli, dir, tag); err != nil {
		return SolutionResult{}, fmt.Errorf("build image: %w", err)
	}

	fmt.Println("Starting container ...")
	containerID, hostPort, err := RunContainer(ctx, cli, tag, manifest.Port)
	if err != nil {
		return SolutionResult{}, fmt.Errorf("run container: %w", err)
	}

	defer func() {
		fmt.Println("Stopping container ...")
		if err := StopContainer(ctx, cli, containerID); err != nil {
			fmt.Fprintf(os.Stderr, "stop container %s: %v\n", containerID, err)
		}
	}()

	timeoutMs := manifest.StartupTimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 10000
	}
	fmt.Printf("Waiting for service on port %d ...\n", hostPort)
	if err := WaitHealthy(ctx, hostPort, timeoutMs); err != nil {
		return SolutionResult{}, fmt.Errorf("wait healthy: %w", err)
	}
	fmt.Println("Service ready.")

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", hostPort)

	fmt.Println("Validating spec compliance ...")
	if err := validateSpec(baseURL); err != nil {
		return SolutionResult{}, fmt.Errorf("spec validation failed: %w", err)
	}
	fmt.Println("Spec validation passed.")

	result := SolutionResult{Name: manifest.Name}

	scenarios := defaultScenarios
	for i := range scenarios {
		scenarios[i].Duration = duration
		scenarios[i].Warmup = warmup
	}

	aborted := false
	for _, cfg := range scenarios {
		fmt.Printf("  Running scenario: %s ...\n", cfg.ID)
		sc, err := runScenario(baseURL, cfg)
		if err != nil {
			fmt.Printf("    TIMEOUT: %v — aborting remaining scenarios\n", err)
			aborted = true
			break
		}
		result.Scenarios = append(result.Scenarios, sc)
		fmt.Printf("    %.0f rps, p99=%.1fms, errors=%.2f%%, conflicts=%.2f%%\n",
			sc.RPS, sc.P99Ms, sc.ErrorRate*100, sc.ConflictRate*100)
		if sc.LeakedLocks > 0 {
			fmt.Printf("    WARNING: %d locks leaked after scenario\n", sc.LeakedLocks)
		}
		time.Sleep(2 * time.Second)
	}

	if !aborted {
		fmt.Println("  Running scenario: list_heavy ...")
		sc, err := runListHeavy(baseURL, warmup, duration)
		if err != nil {
			fmt.Printf("    TIMEOUT: %v — aborting\n", err)
		} else {
			result.Scenarios = append(result.Scenarios, sc)
			fmt.Printf("    %.0f rps, p99=%.1fms, errors=%.2f%%, conflicts=%.2f%%\n",
				sc.RPS, sc.P99Ms, sc.ErrorRate*100, sc.ConflictRate*100)
			if sc.LeakedLocks > 0 {
				fmt.Printf("    WARNING: %d locks leaked after scenario\n", sc.LeakedLocks)
			}
		}
	}

	return result, nil
}
