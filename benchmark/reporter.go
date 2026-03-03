package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// WriteJSON writes the full report as JSON to the given path.
func WriteJSON(report Report, path string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// WriteMarkdown writes a comparison table as Markdown to the given path.
// Rows = scenarios, columns = solutions, cells = "RPS / P99ms".
func WriteMarkdown(report Report, path string) error {
	var sb strings.Builder

	sb.WriteString("# Benchmark Report\n\n")
	sb.WriteString(fmt.Sprintf("**Timestamp:** %s\n\n", report.Timestamp))

	if len(report.Solutions) == 0 {
		sb.WriteString("No solutions benchmarked.\n")
		return os.WriteFile(path, []byte(sb.String()), 0644)
	}

	// Collect all scenario IDs in order from the first solution.
	var scenarioIDs []string
	for _, sc := range report.Solutions[0].Scenarios {
		scenarioIDs = append(scenarioIDs, sc.ScenarioID)
	}

	// Build index: solution name → scenario ID → result
	index := map[string]map[string]ScenarioResult{}
	for _, sol := range report.Solutions {
		index[sol.Name] = map[string]ScenarioResult{}
		for _, sc := range sol.Scenarios {
			index[sol.Name][sc.ScenarioID] = sc
		}
	}

	// Header row
	sb.WriteString("## Results (RPS / P99 ms)\n\n")
	sb.WriteString("| Scenario |")
	for _, sol := range report.Solutions {
		sb.WriteString(fmt.Sprintf(" %s |", sol.Name))
	}
	sb.WriteString("\n")

	// Separator
	sb.WriteString("|---|")
	for range report.Solutions {
		sb.WriteString("---|")
	}
	sb.WriteString("\n")

	// Data rows
	for _, id := range scenarioIDs {
		sb.WriteString(fmt.Sprintf("| %s |", id))
		for _, sol := range report.Solutions {
			sc, ok := index[sol.Name][id]
			if !ok {
				sb.WriteString(" — |")
				continue
			}
			sb.WriteString(fmt.Sprintf(" %.0f RPS / %.1f ms |", sc.RPS, sc.P99Ms))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n## Detailed Metrics\n\n")

	for _, sol := range report.Solutions {
		sb.WriteString(fmt.Sprintf("### %s\n\n", sol.Name))
		sb.WriteString("| Scenario | Requests | RPS | P50 ms | P95 ms | P99 ms | Max ms | Error% | Conflict% |\n")
		sb.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, sc := range sol.Scenarios {
			sb.WriteString(fmt.Sprintf("| %s | %d | %.0f | %.1f | %.1f | %.1f | %.1f | %.2f%% | %.2f%% |\n",
				sc.ScenarioID,
				sc.RequestsTotal,
				sc.RPS,
				sc.P50Ms,
				sc.P95Ms,
				sc.P99Ms,
				sc.MaxMs,
				sc.ErrorRate*100,
				sc.ConflictRate*100,
			))
		}
		sb.WriteString("\n")
	}

	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// PrintSummary prints a compact comparison table to stdout.
func PrintSummary(report Report) {
	if len(report.Solutions) == 0 {
		fmt.Println("No results to display.")
		return
	}

	var scenarioIDs []string
	for _, sc := range report.Solutions[0].Scenarios {
		scenarioIDs = append(scenarioIDs, sc.ScenarioID)
	}

	// Column widths
	const scenarioW = 18
	const colW = 22

	// Header
	fmt.Printf("\n%-*s", scenarioW, "Scenario")
	for _, sol := range report.Solutions {
		name := sol.Name
		if len(name) > colW-2 {
			name = name[:colW-2]
		}
		fmt.Printf("  %-*s", colW-2, name)
	}
	fmt.Println()

	fmt.Printf("%-*s", scenarioW, strings.Repeat("-", scenarioW))
	for range report.Solutions {
		fmt.Printf("  %-*s", colW-2, strings.Repeat("-", colW-2))
	}
	fmt.Println()

	index := map[string]map[string]ScenarioResult{}
	for _, sol := range report.Solutions {
		index[sol.Name] = map[string]ScenarioResult{}
		for _, sc := range sol.Scenarios {
			index[sol.Name][sc.ScenarioID] = sc
		}
	}

	for _, id := range scenarioIDs {
		fmt.Printf("%-*s", scenarioW, id)
		for _, sol := range report.Solutions {
			sc, ok := index[sol.Name][id]
			if !ok {
				fmt.Printf("  %-*s", colW-2, "—")
				continue
			}
			cell := fmt.Sprintf("%.0f rps / %.1fms", sc.RPS, sc.P99Ms)
			fmt.Printf("  %-*s", colW-2, cell)
		}
		fmt.Println()
	}
	fmt.Println()
}
