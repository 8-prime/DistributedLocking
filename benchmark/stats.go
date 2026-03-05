package main

import (
	"context"
	"errors"
	"net"
	"sort"
)

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

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
