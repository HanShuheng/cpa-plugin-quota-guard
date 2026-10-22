// Package main implements the codex-quota-guard CLIProxyAPI plugin: it
// watches Codex OAuth credentials, queries their live ChatGPT quota windows
// upstream, and disables a credential when a configured quota floor is
// reached so the remaining quota is reserved for direct use. When the window
// recovers (regular reset, a manual reset, or an official grant), the
// credential is re-enabled automatically.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

//go:embed web/page.html
var pageHTML []byte

const (
	pluginID   = "codex-quota-guard"
	usageURL   = "https://chatgpt.com/backend-api/wham/usage"
	userAgent  = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	stopWait   = 10 * time.Second
	httpBudget = 25 * time.Second
)

// AccountRule overrides the global floors for one credential. Match is a
// case-insensitive substring of the credential file name, label or email.
type AccountRule struct {
	Match                 string  `yaml:"match" json:"match"`
	PrimaryMinRemaining   float64 `yaml:"primary-min-remaining,omitempty" json:"primary-min-remaining,omitempty"`
	SecondaryMinRemaining float64 `yaml:"secondary-min-remaining,omitempty" json:"secondary-min-remaining,omitempty"`
}

// Config is the plugin subtree under plugins.configs.codex-quota-guard.
// Nothing is guarded until an account is listed (or protect-all-codex is on).
type Config struct {
	Enabled               bool          `yaml:"enabled" json:"enabled"`
	PollIntervalSeconds   int           `yaml:"poll-interval-seconds" json:"poll-interval-seconds"`
	ProtectAllCodex       bool          `yaml:"protect-all-codex" json:"protect-all-codex"`
	PrimaryMinRemaining   float64       `yaml:"primary-min-remaining" json:"primary-min-remaining"`
	SecondaryMinRemaining float64       `yaml:"secondary-min-remaining" json:"secondary-min-remaining"`
	Accounts              []AccountRule `yaml:"accounts" json:"accounts"`
	Exclude               []string      `yaml:"exclude" json:"exclude"`
}

func defaultConfig() Config {
	return Config{
		Enabled:               true,
		PollIntervalSeconds:   300,
		PrimaryMinRemaining:   10,
		SecondaryMinRemaining: 5,
	}
}

// AccountState is the persisted per-credential guard ownership. A credential
// the guard disabled is re-enabled only by the guard; a manual disable is
// never touched.
type AccountState struct {
	Disabled   bool              `json:"disabled"` // the guard disabled this credential
	Breaches   []string          `json:"breaches"` // windows below their floor at disable time
	Resets     map[string]string `json:"resets"`   // window -> RFC3339 reset observed at disable time
	DisabledAt string            `json:"disabled_at"`
}

type PersistedState struct {
	Version  string                  `json:"version"`
	Accounts map[string]AccountState `json:"accounts"` // keyed by credential file name
}

// AccountStatus is one credential's latest observation.
type AccountStatus struct {
	Name                 string  `json:"name"`
	AuthIndex            string  `json:"auth_index,omitempty"`
	Source               string  `json:"source,omitempty"`
	Protected            bool    `json:"protected"`
	Disabled             bool    `json:"disabled"`
	GuardOwned           bool    `json:"guard_owned"`
	FiveHourRemainingPct float64 `json:"five_hour_remaining_percent"`
	WeeklyRemainingPct   float64 `json:"weekly_remaining_percent"`
	FiveHourReset        string  `json:"five_hour_reset,omitempty"`
	WeeklyReset          string  `json:"weekly_reset,omitempty"`
	FetchError           string  `json:"fetch_error,omitempty"`
	LastAction           string  `json:"last_action,omitempty"`
	ObservedAt           string  `json:"observed_at,omitempty"`
}

// Caller sends one host callback and returns the raw envelope bytes.
type Caller func(method string, payload []byte) ([]byte, error)

type Guard struct {
	host    Caller
	version string
	dataDir string

	mu      sync.Mutex
	cfg     Config
	cfgErr  error
	status  map[string]AccountStatus // keyed by credential file name
	state   PersistedState
	started bool
	stopped bool
	stop    chan struct{}
	done    chan struct{}
}

func NewGuard(host Caller, version string) *Guard {
	return &Guard{
		host:    host,
		version: version,
		dataDir: resolveDataDir(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		status:  map[string]AccountStatus{},
	}
}

// ---- host RPC ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status,omitempty"`
	} `json:"error,omitempty"`
}

func (g *Guard) call(method string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := g.host(method, raw)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if len(resp) == 0 {
		return fmt.Errorf("%s: empty response", method)
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return fmt.Errorf("%s: invalid envelope: %w", method, err)
	}
	if !env.OK {
		msg := "callback failed"
		if env.Error != nil {
			msg = env.Error.Code + ": " + env.Error.Message
		}
		return fmt.Errorf("%s: %s", method, msg)
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func (g *Guard) log(level, message string) {
	_ = g.call(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  map[string]any{"plugin": pluginID},
	}, nil)
}

// httpDo issues one HTTP request through the host client (proxy-aware).
func (g *Guard) httpDo(method, url string, header http.Header, body []byte) (pluginapi.HTTPResponse, error) {
	var opened struct {
		OperationID string `json:"operation_id"`
	}
	if err := g.call(pluginabi.MethodHostHTTPOperationOpen, map[string]any{}, &opened); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	operationID := strings.TrimSpace(opened.OperationID)
	if operationID == "" {
		return pluginapi.HTTPResponse{}, fmt.Errorf("empty operation id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpBudget)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer func() { _ = recover() }()
		<-ctx.Done()
		select {
		case <-done:
		default:
			_ = g.call(pluginabi.MethodHostHTTPCancel, map[string]any{"operation_id": operationID}, nil)
		}
	}()

	var resp pluginapi.HTTPResponse
	err := g.call(pluginabi.MethodHostHTTPDo, map[string]any{
		"operation_id": operationID,
		"method":       method,
		"url":          url,
		"headers":      header,
		"body":         body,
	}, &resp)
	return resp, err
}

// ---- data dir (plugins/data/<id>) ----

func resolveDataDir() string {
	workDir, _ := os.Getwd()
	hostConfig := hostConfigPath(os.Args, workDir)
	raw, _ := os.ReadFile(hostConfig)
	dir := "plugins"
	var cfg struct {
		Plugins struct {
			Dir string `yaml:"dir"`
		} `yaml:"plugins"`
	}
	if yaml.Unmarshal(raw, &cfg) == nil && strings.TrimSpace(cfg.Plugins.Dir) != "" {
		dir = strings.TrimSpace(cfg.Plugins.Dir)
	}
	if strings.HasPrefix(dir, "~") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[1:])
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(hostConfig), dir)
	}
	return filepath.Join(dir, "data", pluginID)
}

func hostConfigPath(args []string, workDir string) string {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name != "config" {
			continue
		}
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return filepath.Join(workDir, "config.yaml")
}

func (g *Guard) statePath() string { return filepath.Join(g.dataDir, "state.json") }

func (g *Guard) loadState() PersistedState {
	s := PersistedState{Version: "1", Accounts: map[string]AccountState{}}
	if raw, err := os.ReadFile(g.statePath()); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	if s.Accounts == nil {
		s.Accounts = map[string]AccountState{}
	}
	return s
}

func (g *Guard) persistState() {
	if err := os.MkdirAll(g.dataDir, 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(g.state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(g.statePath(), raw, 0o644)
}

// ---- lifecycle ----

func (g *Guard) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = errorEnvelope("plugin_panic", fmt.Sprint(r))
		}
	}()
	result, err := g.handle(method, request)
	if err != nil {
		return errorEnvelope("plugin_error", err.Error())
	}
	return result
}

func ok(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{Error: &struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status,omitempty"`
	}{Code: code, Message: message}})
	return raw
}

func (g *Guard) handle(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cfg := defaultConfig()
		err := yaml.Unmarshal(req.ConfigYAML, &cfg)
		if cfg.PollIntervalSeconds <= 0 {
			cfg.PollIntervalSeconds = defaultConfig().PollIntervalSeconds
		}
		g.mu.Lock()
		g.cfg, g.cfgErr = cfg, err
		shouldStart := !g.started && !g.stopped && cfg.Enabled && err == nil
		if shouldStart {
			g.started = true
			g.state = g.loadState()
		}
		g.mu.Unlock()
		if shouldStart {
			go g.loop()
		}
		return ok(g.registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		g.Shutdown()
		return ok(struct{}{})
	case pluginabi.MethodManagementRegister:
		return ok(managementRegistration())
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return ok(g.handleManagement(req))
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func (g *Guard) Shutdown() {
	g.mu.Lock()
	if g.stopped || !g.started {
		g.mu.Unlock()
		return
	}
	g.stopped = true
	close(g.stop)
	g.mu.Unlock()
	select {
	case <-g.done:
	case <-time.After(stopWait):
	}
}

// repositoryURL 是插件源码仓库；宿主的 validPlugin 要求非空，发布前改成实际地址。
const repositoryURL = "https://github.com/HanShuheng/cpa-plugin-quota-guard"

func (g *Guard) registration() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "Codex Quota Guard",
			Version:          g.version,
			Author:           "hanshuheng",
			GitHubRepository: repositoryURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "poll-interval-seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "轮询间隔秒数 / Poll interval in seconds"},
				{Name: "protect-all-codex", Type: pluginapi.ConfigFieldTypeBoolean, Description: "保护全部 Codex OAuth 凭证（默认关闭，只保护 accounts 列出的）/ Guard every Codex OAuth credential (default off; only listed accounts are guarded)"},
				{Name: "primary-min-remaining", Type: pluginapi.ConfigFieldTypeNumber, Description: "全局默认：5 小时窗口剩余百分比低于此值时禁用 / Global default floor for the 5-hour window, in percent remaining"},
				{Name: "secondary-min-remaining", Type: pluginapi.ConfigFieldTypeNumber, Description: "全局默认：周窗口剩余百分比低于此值时禁用 / Global default floor for the weekly window, in percent remaining"},
				{Name: "accounts", Type: pluginapi.ConfigFieldTypeObject, Description: "受保护凭证列表，每项含 match（文件名/邮箱子串）及可选阈值覆盖 / Guarded credentials: match (file-name/email substring) plus optional floor overrides"},
				{Name: "exclude", Type: pluginapi.ConfigFieldTypeObject, Description: "排除的凭证子串列表（配合 protect-all-codex）/ Excluded credential substrings (with protect-all-codex)"},
			},
		},
		"capabilities": map[string]bool{"management_api": true},
	}
}

type mgmtRoute struct {
	Method      string
	Path        string
	Description string
}

type mgmtResource struct {
	Path        string
	Menu        string
	Description string
}

func managementRegistration() map[string]any {
	base := "/v0/management/" + pluginID
	return map[string]any{
		"routes": []mgmtRoute{
			{Method: http.MethodGet, Path: base + "/status", Description: "List discovered Codex credentials, live quota and guard state"},
			{Method: http.MethodPost, Path: base + "/check", Description: "Run one guard pass now"},
			{Method: http.MethodGet, Path: base + "/page", Description: "Management page"},
		},
		"resources": []mgmtResource{
			{Path: "/page", Menu: "Codex Quota Guard", Description: "Codex 额度保底守卫：窗口触底自动禁用、恢复自动启用 / Quota floor guard for Codex credentials"},
		},
	}
}

func (g *Guard) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	base := "/v0/management/" + pluginID
	switch {
	case req.Method == http.MethodGet && req.Path == base+"/status":
		g.mu.Lock()
		cfg := g.cfg
		statuses := make([]AccountStatus, 0, len(g.status))
		for _, s := range g.status {
			statuses = append(statuses, s)
		}
		g.mu.Unlock()
		body, _ := json.MarshalIndent(map[string]any{
			"config":   cfg,
			"accounts": statuses,
		}, "", "  ")
		return jsonResponse(http.StatusOK, body)
	case req.Method == http.MethodPost && req.Path == base+"/check":
		g.tick()
		g.mu.Lock()
		statuses := make([]AccountStatus, 0, len(g.status))
		for _, s := range g.status {
			statuses = append(statuses, s)
		}
		g.mu.Unlock()
		body, _ := json.MarshalIndent(map[string]any{"accounts": statuses}, "", "  ")
		return jsonResponse(http.StatusOK, body)
	case req.Method == http.MethodGet && req.Path == base+"/page":
		resp := pluginapi.ManagementResponse{StatusCode: http.StatusOK, Body: pageHTML}
		resp.Headers = http.Header{}
		resp.Headers.Set("Content-Type", "text/html; charset=utf-8")
		return resp
	default:
		return jsonResponse(http.StatusNotFound, []byte(`{"error":"not found"}`))
	}
}

func jsonResponse(status int, body []byte) pluginapi.ManagementResponse {
	resp := pluginapi.ManagementResponse{StatusCode: status, Body: body}
	resp.Headers = http.Header{}
	resp.Headers.Set("Content-Type", "application/json")
	return resp
}

// ---- the guard loop ----

func (g *Guard) loop() {
	defer close(g.done)
	// 注册发生在核心启动早期，凭证索引可能尚未就绪，首个检查稍作延迟。
	select {
	case <-g.stop:
		return
	case <-time.After(15 * time.Second):
	}
	g.tick()
	for {
		g.mu.Lock()
		interval := time.Duration(g.cfg.PollIntervalSeconds) * time.Second
		g.mu.Unlock()
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		timer := time.NewTimer(interval)
		select {
		case <-g.stop:
			timer.Stop()
			return
		case <-timer.C:
			g.tick()
		}
	}
}

// ruleFor returns the effective floors for a credential, or ok=false when the
// credential is not guarded.
func (g *Guard) ruleFor(cfg Config, name, label string) (float64, float64, bool) {
	if matchesAny(cfg.Exclude, name, label) {
		return 0, 0, false
	}
	for _, rule := range cfg.Accounts {
		if rule.Match != "" && matches(rule.Match, name, label) {
			p, s := cfg.PrimaryMinRemaining, cfg.SecondaryMinRemaining
			if rule.PrimaryMinRemaining > 0 {
				p = rule.PrimaryMinRemaining
			}
			if rule.SecondaryMinRemaining > 0 {
				s = rule.SecondaryMinRemaining
			}
			return p, s, true
		}
	}
	if cfg.ProtectAllCodex {
		return cfg.PrimaryMinRemaining, cfg.SecondaryMinRemaining, true
	}
	return 0, 0, false
}

func matches(pattern, name, label string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	return p != "" && (strings.Contains(strings.ToLower(name), p) || strings.Contains(strings.ToLower(label), p))
}

func matchesAny(patterns []string, name, label string) bool {
	for _, pattern := range patterns {
		if matches(pattern, name, label) {
			return true
		}
	}
	return false
}
