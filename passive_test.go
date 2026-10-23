package main

import (
	"net/http"
	"testing"
)

func TestUsageFromHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("X-Codex-Primary-Used-Percent", "40")
	header.Set("X-Codex-Secondary-Used-Percent", "98")
	header.Set("X-Codex-Primary-Reset-At", "1792707014")
	usage, ok := usageFromHeaders(header)
	if !ok {
		t.Fatalf("usageFromHeaders should succeed with used-percent headers")
	}
	if usage.FiveHourRemaining != 60 || usage.WeeklyRemaining != 2 {
		t.Fatalf("remaining = %v/%v, want 60/2", usage.FiveHourRemaining, usage.WeeklyRemaining)
	}
	if usage.FiveHourReset == "" {
		t.Fatalf("primary reset should be parsed from epoch")
	}
	if _, ok := usageFromHeaders(http.Header{}); ok {
		t.Fatalf("empty headers should not parse")
	}
}
