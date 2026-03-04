package main

type SolutionManifest struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	Port             int    `json:"port"`
	StartupTimeoutMs int    `json:"startupTimeoutMs"`
}

type ScenarioResult struct {
	ScenarioID    string  `json:"scenarioId"`
	RequestsTotal int64   `json:"requestsTotal"`
	DurationMs    int64   `json:"durationMs"`
	RPS           float64 `json:"rps"`
	P50Ms         float64 `json:"p50Ms"`
	P95Ms         float64 `json:"p95Ms"`
	P99Ms         float64 `json:"p99Ms"`
	MaxMs         float64 `json:"maxMs"`
	ErrorRate     float64 `json:"errorRate"`
	ConflictRate  float64 `json:"conflictRate"`
	LeakedLocks   int     `json:"leakedLocks"`
}

type SolutionResult struct {
	Name      string           `json:"name"`
	Scenarios []ScenarioResult `json:"scenarios"`
}

type Report struct {
	Timestamp string           `json:"timestamp"`
	Solutions []SolutionResult `json:"solutions"`
}
