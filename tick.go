package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// tick runs one full pass: discover Codex OAuth credentials, fetch live
// usage for each, and enforce the configured floors on guarded ones.
func (g *Guard) tick() {
	g.mu.Lock()
	cfg := g.cfg
	if cfgErr := g.cfgErr; cfgErr != nil {
		g.mu.Unlock()
		g.log("warn", fmt.Sprintf("配置解析失败，本轮跳过: %v", cfgErr))
		return
	}
	g.mu.Unlock()

	var listResp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := g.call(pluginabi.MethodHostAuthList, map[string]any{}, &listResp); err != nil {
		g.log("warn", "host.auth.list 失败: "+err.Error())
		return
	}

	next := map[string]AccountStatus{}
	for _, entry := range listResp.Files {
		if !isCodexOAuth(entry) {
			continue
		}
		status := g.checkOne(cfg, entry)
		next[entry.Name] = status
	}

	g.mu.Lock()
	g.status = next
	g.mu.Unlock()
}

// isCodexOAuth 识别文件型 Codex OAuth 凭证：宿主侧 Type/Provider 填的是
// 凭证 JSON 的 type 字段（"codex"），OAuth 凭证文件以 .json 结尾
// （API-key 凭证状态文件是 .cds，不在此列）。
func isCodexOAuth(entry pluginapi.HostAuthFileEntry) bool {
	provider := entry.Provider
	if provider == "" {
		provider = entry.Type
	}
	if provider != "codex" {
		return false
	}
	// OAuth 凭证文件以 .json 结尾；API-key 凭证状态文件是 .cds，不在此列。
	return strings.HasSuffix(strings.ToLower(entry.Name), ".json")
}

// checkOne fetches one credential's live usage and enforces its floors.
func (g *Guard) checkOne(cfg Config, entry pluginapi.HostAuthFileEntry) AccountStatus {
	status := AccountStatus{
		Name:       entry.Name,
		AuthIndex:  entry.AuthIndex,
		Source:     entry.Source,
		Disabled:   entry.Disabled,
		ObservedAt: time.Now().Format(time.RFC3339),
	}
	primaryMin, secondaryMin, protected := g.ruleFor(cfg, entry.Name, entry.Label)
	status.Protected = protected

	var getResp pluginapi.HostAuthGetResponse
	if err := g.call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: entry.AuthIndex}, &getResp); err != nil {
		status.FetchError = "auth.get: " + err.Error()
		g.setStatus(status)
		return status
	}

	usage, err := g.fetchUsage(getResp.JSON)
	if err != nil {
		status.FetchError = err.Error()
		// Fetch failed: fall back to reset-time recovery for guard-owned
		// disables so a long outage cannot pin a usable credential.
		if protected && entry.Disabled {
			g.recoverByResetTime(entry)
		}
		g.setStatus(status)
		return status
	}

	status.FiveHourRemainingPct = usage.FiveHourRemaining
	status.WeeklyRemainingPct = usage.WeeklyRemaining
	status.FiveHourReset = usage.FiveHourReset
	status.WeeklyReset = usage.WeeklyReset

	if !protected {
		g.setStatus(status)
		return status
	}

	g.mu.Lock()
	prev := g.state.Accounts[entry.Name]
	g.mu.Unlock()
	guardOwned := prev.Disabled

	if !entry.Disabled {
		var breaches []string
		resets := map[string]string{}
		if primaryMin > 0 && usage.FiveHourRemaining <= primaryMin {
			breaches = append(breaches, "primary")
			resets["primary"] = usage.FiveHourReset
		}
		if secondaryMin > 0 && usage.WeeklyRemaining <= secondaryMin {
			breaches = append(breaches, "secondary")
			resets["secondary"] = usage.WeeklyReset
		}
		if len(breaches) == 0 {
			g.setStatus(status)
			return status
		}
		if g.saveDisabled(entry.Name, getResp.JSON, true) {
			now := time.Now().Format(time.RFC3339)
			g.mu.Lock()
			g.state.Accounts[entry.Name] = AccountState{Disabled: true, Breaches: breaches, Resets: resets, DisabledAt: now}
			g.persistState()
			g.mu.Unlock()
			status.Disabled = true
			status.GuardOwned = true
			status.LastAction = "disabled: " + fmt.Sprintf("5h剩余%.0f%%(下限%.0f%%) 周剩余%.0f%%(下限%.0f%%)",
				usage.FiveHourRemaining, primaryMin, usage.WeeklyRemaining, secondaryMin)
			g.log("warn", fmt.Sprintf("⛔ 已禁用 %s：%s", entry.Name, status.LastAction))
		} else {
			status.LastAction = "disable failed"
		}
		g.setStatus(status)
		return status
	}

	// Credential is disabled.
	if !guardOwned {
		status.LastAction = "manual disable, skipped"
		g.setStatus(status)
		return status
	}
	primaryOK := primaryMin <= 0 || usage.FiveHourRemaining > primaryMin
	secondaryOK := secondaryMin <= 0 || usage.WeeklyRemaining > secondaryMin
	if !primaryOK || !secondaryOK {
		status.GuardOwned = true
		status.LastAction = "holding disabled"
		g.setStatus(status)
		return status
	}
	if g.saveDisabled(entry.Name, getResp.JSON, false) {
		g.mu.Lock()
		delete(g.state.Accounts, entry.Name)
		g.persistState()
		g.mu.Unlock()
		status.Disabled = false
		status.LastAction = fmt.Sprintf("re-enabled: 5h剩余%.0f%% 周剩余%.0f%%", usage.FiveHourRemaining, usage.WeeklyRemaining)
		g.log("info", fmt.Sprintf("▶️ 已恢复启用 %s：%s", entry.Name, status.LastAction))
	} else {
		status.GuardOwned = true
		status.LastAction = "enable failed"
	}
	g.setStatus(status)
	return status
}

// setStatus merges one credential's status into the shared map.
func (g *Guard) setStatus(status AccountStatus) {
	g.mu.Lock()
	g.status[status.Name] = status
	g.mu.Unlock()
}

// saveDisabled persists the credential JSON with the disabled flag flipped.
func (g *Guard) saveDisabled(name string, raw json.RawMessage, disabled bool) bool {
	var cred map[string]any
	if err := json.Unmarshal(raw, &cred); err != nil {
		g.log("warn", "解析凭证 JSON 失败 "+name+": "+err.Error())
		return false
	}
	if v, ok := cred["disabled"].(bool); ok && v == disabled {
		return true
	}
	cred["disabled"] = disabled
	payload, err := json.Marshal(cred)
	if err != nil {
		return false
	}
	var saveResp pluginapi.HostAuthSaveResponse
	err = g.call(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: payload,
	}, &saveResp)
	if err != nil {
		g.log("warn", fmt.Sprintf("保存凭证失败 %s (disabled=%v): %v", name, disabled, err))
		return false
	}
	return true
}

// recoverByResetTime re-enables a guard-owned disable when every breached
// window's observed reset deadline has passed. This is the fallback when the
// live usage fetch keeps failing (for example a long proxy outage).
func (g *Guard) recoverByResetTime(entry pluginapi.HostAuthFileEntry) {
	g.mu.Lock()
	prev, owned := g.state.Accounts[entry.Name]
	g.mu.Unlock()
	if !owned {
		return
	}
	now := time.Now()
	for _, reset := range prev.Resets {
		t, err := time.Parse(time.RFC3339, reset)
		if err != nil || now.Before(t) {
			return
		}
	}
	var getResp pluginapi.HostAuthGetResponse
	if err := g.call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: entry.AuthIndex}, &getResp); err != nil {
		return
	}
	if g.saveDisabled(entry.Name, getResp.JSON, false) {
		g.mu.Lock()
		delete(g.state.Accounts, entry.Name)
		g.persistState()
		g.mu.Unlock()
		g.log("info", fmt.Sprintf("▶️ 额度查询持续失败但触底窗口已过重置时间，恢复启用 %s", entry.Name))
	}
}

// ---- live quota fetch ----

type liveUsage struct {
	FiveHourRemaining float64
	WeeklyRemaining   float64
	FiveHourReset     string
	WeeklyReset       string
}

type parsedCredential struct {
	AccessToken string
	AccountID   string
}

func parseCredential(raw json.RawMessage) parsedCredential {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return parsedCredential{}
	}
	out := parsedCredential{}
	if token, ok := data["access_token"].(string); ok && token != "" {
		out.AccessToken = token
	}
	if metadata, ok := data["metadata"].(map[string]any); ok {
		if id, ok := metadata["account_id"].(string); ok {
			out.AccountID = id
		}
		if out.AccessToken == "" {
			if token, ok := metadata["access_token"].(string); ok {
				out.AccessToken = token
			}
		}
	}
	return out
}

// fetchUsage queries the live ChatGPT usage endpoint for one credential.
// This works whether the credential is enabled or disabled, so an early
// recovery (manual reset card, official grant) is detected immediately.
func (g *Guard) fetchUsage(raw json.RawMessage) (liveUsage, error) {
	cred := parseCredential(raw)
	if cred.AccessToken == "" {
		return liveUsage{}, fmt.Errorf("credential has no access_token")
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.AccessToken)
	header.Set("User-Agent", userAgent)
	if cred.AccountID != "" {
		header.Set("Chatgpt-Account-Id", cred.AccountID)
	}
	resp, err := g.httpDo(http.MethodGet, usageURL, header, nil)
	if err != nil {
		return liveUsage{}, fmt.Errorf("usage request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return liveUsage{}, fmt.Errorf("usage HTTP %d", resp.StatusCode)
	}
	return parseUsage(resp.Body)
}

func parseUsage(body []byte) (liveUsage, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return liveUsage{}, fmt.Errorf("usage decode: %w", err)
	}
	rateLimit, _ := payload["rate_limit"].(map[string]any)
	if rateLimit == nil {
		rateLimit, _ = payload["rateLimit"].(map[string]any)
	}
	if rateLimit == nil {
		return liveUsage{}, fmt.Errorf("usage response has no rate_limit")
	}
	primary, _ := rateLimit["primary_window"].(map[string]any)
	secondary, _ := rateLimit["secondary_window"].(map[string]any)
	if primary == nil {
		primary, _ = rateLimit["primaryWindow"].(map[string]any)
	}
	if secondary == nil {
		secondary, _ = rateLimit["secondaryWindow"].(map[string]any)
	}

	five := windowByDuration(primary, 18000)
	week := windowByDuration(secondary, 604800)
	if five == nil {
		five = primary
	}
	if week == nil {
		week = secondary
	}

	out := liveUsage{FiveHourRemaining: 100, WeeklyRemaining: 100}
	if five != nil {
		out.FiveHourRemaining = remainingPercent(five)
		out.FiveHourReset = resetTime(five)
	}
	if week != nil {
		out.WeeklyRemaining = remainingPercent(week)
		out.WeeklyReset = resetTime(week)
	}
	return out, nil
}

// windowByDuration returns win when it matches the expected window length in
// seconds, else nil. Both windows report limit_window_seconds.
func windowByDuration(win map[string]any, wantSeconds float64) map[string]any {
	if win == nil {
		return nil
	}
	if seconds, ok := number(win["limit_window_seconds"]); !ok || seconds != wantSeconds {
		if seconds2, ok := number(win["limitWindowSeconds"]); !ok || seconds2 != wantSeconds {
			return nil
		}
	}
	return win
}

func remainingPercent(win map[string]any) float64 {
	if used, ok := number(win["used_percent"]); ok {
		return clamp(100 - used)
	}
	if used, ok := number(win["usedPercent"]); ok {
		return clamp(100 - used)
	}
	if reached, ok := win["limit_reached"].(bool); ok && reached {
		return 0
	}
	if reached, ok := win["limitReached"].(bool); ok && reached {
		return 0
	}
	return 100
}

func resetTime(win map[string]any) string {
	if s, ok := win["reset_at"].(string); ok && s != "" {
		return s
	}
	if s, ok := win["resetAt"].(string); ok && s != "" {
		return s
	}
	if after, ok := number(win["reset_after_seconds"]); ok {
		return time.Now().Add(time.Duration(after * float64(time.Second))).UTC().Format(time.RFC3339)
	}
	return ""
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
