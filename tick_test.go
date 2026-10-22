package main

import (
	"encoding/json"
	"testing"
)

func TestParseUsageWindows(t *testing.T) {
	body := []byte(`{"rate_limit":{
		"primary_window":{"used_percent":51,"limit_window_seconds":18000,"reset_at":"2026-10-22T13:45:00Z"},
		"secondary_window":{"used_percent":98,"limit_window_seconds":604800,"reset_at":"2026-10-27T06:35:00Z"}
	}}`)
	usage, err := parseUsage(body)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if usage.FiveHourRemaining != 49 {
		t.Errorf("5h remaining = %v, want 49", usage.FiveHourRemaining)
	}
	if usage.WeeklyRemaining != 2 {
		t.Errorf("weekly remaining = %v, want 2", usage.WeeklyRemaining)
	}
	if usage.FiveHourReset != "2026-10-22T13:45:00Z" || usage.WeeklyReset != "2026-10-27T06:35:00Z" {
		t.Errorf("reset times = %v / %v", usage.FiveHourReset, usage.WeeklyReset)
	}
}

func TestParseUsageCamelCaseAndLimitReached(t *testing.T) {
	body := []byte(`{"rateLimit":{
		"primaryWindow":{"usedPercent":40,"limitWindowSeconds":18000,"resetAfterSeconds":600},
		"secondaryWindow":{"limitReached":true,"limitWindowSeconds":604800,"resetAt":"2026-10-27T06:35:00Z"}
	}}`)
	usage, err := parseUsage(body)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if usage.FiveHourRemaining != 60 {
		t.Errorf("5h remaining = %v, want 60", usage.FiveHourRemaining)
	}
	if usage.WeeklyRemaining != 0 {
		t.Errorf("weekly remaining = %v, want 0 (limit reached)", usage.WeeklyRemaining)
	}
}

func TestParseCredentialTokenLocations(t *testing.T) {
	raw := json.RawMessage(`{"access_token":"tok1","metadata":{"account_id":"acc1"}}`)
	cred := parseCredential(raw)
	if cred.AccessToken != "tok1" || cred.AccountID != "acc1" {
		t.Fatalf("parseCredential = %+v", cred)
	}
	raw = json.RawMessage(`{"metadata":{"access_token":"tok2"}}`)
	if cred = parseCredential(raw); cred.AccessToken != "tok2" {
		t.Fatalf("metadata access_token not read: %+v", cred)
	}
}

func TestRuleForMatchingAndExclusion(t *testing.T) {
	g := &Guard{}
	cfg := Config{
		PrimaryMinRemaining:   10,
		SecondaryMinRemaining: 5,
		Accounts: []AccountRule{
			{Match: "mdb25", PrimaryMinRemaining: 15},
			{Match: "other", PrimaryMinRemaining: -1}, // 负值不覆盖，回落全局
		},
		Exclude: []string{"apikey"},
	}
	name := "codex-2419d47b-mdb25jv772@example.com-plus.json"
	p, s, ok := g.ruleFor(cfg, name, "")
	if !ok || p != 15 || s != 5 {
		t.Fatalf("mdb rule = %v/%v/%v, want 15/5/true", p, s, ok)
	}
	if _, _, ok := g.ruleFor(cfg, "codex-apikey-x.json", ""); ok {
		t.Fatalf("excluded credential should not be guarded")
	}
	if _, _, ok := g.ruleFor(cfg, "codex-plain.json", ""); ok {
		t.Fatalf("unlisted credential must not be guarded by default")
	}
	cfg.ProtectAllCodex = true
	if p, s, ok := g.ruleFor(cfg, "codex-plain.json", ""); !ok || p != 10 || s != 5 {
		t.Fatalf("protect-all = %v/%v/%v, want 10/5/true", p, s, ok)
	}
}
