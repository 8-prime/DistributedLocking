package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

func main() {
	warmupFlag := flag.Duration("warmup", 5*time.Second, "warmup duration per scenario")
	durationFlag := flag.Duration("duration", 15*time.Second, "measurement duration per scenario")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: benchmark [--duration 15s] [--warmup 5s] <solutions-dir|solution-dir> ...")
		os.Exit(1)
	}

	// Discover solution directories from the given paths.
	var solutionDirs []string
	for _, arg := range args {
		dirs, err := discoverSolutions(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "discover solutions in %s: %v\n", arg, err)
			os.Exit(1)
		}
		solutionDirs = append(solutionDirs, dirs...)
	}

	if len(solutionDirs) == 0 {
		fmt.Fprintln(os.Stderr, "no solutions found")
		os.Exit(1)
	}

	fmt.Printf("Found %d solution(s):\n", len(solutionDirs))
	for _, d := range solutionDirs {
		fmt.Printf("  %s\n", d)
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Fprintf(os.Stderr, "docker client: %v\n", err)
		os.Exit(1)
	}
	defer cli.Close()

	report := Report{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	for _, dir := range solutionDirs {
		result, err := benchmarkSolution(context.Background(), cli, dir, *warmupFlag, *durationFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "benchmark %s: %v\n", dir, err)
			continue
		}
		report.Solutions = append(report.Solutions, result)
	}

	// Write results.
	if err := os.MkdirAll("../results", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "create results dir: %v\n", err)
	}

	stamp := strings.ReplaceAll(report.Timestamp, ":", "-")
	jsonPath := fmt.Sprintf("../results/benchmark-%s.json", stamp)
	mdPath := fmt.Sprintf("../results/benchmark-%s.md", stamp)

	if err := WriteJSON(report, jsonPath); err != nil {
		fmt.Fprintf(os.Stderr, "write JSON: %v\n", err)
	} else {
		fmt.Printf("JSON report: %s\n", jsonPath)
	}

	if err := WriteMarkdown(report, mdPath); err != nil {
		fmt.Fprintf(os.Stderr, "write Markdown: %v\n", err)
	} else {
		fmt.Printf("Markdown report: %s\n", mdPath)
	}

	PrintSummary(report)
}
