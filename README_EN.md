# Codex Quota Guard (CPA plugin)

English | [中文](README.md)

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that enforces a **quota floor** on Codex (ChatGPT) OAuth credentials. When a credential's 5-hour or weekly window remaining quota approaches the configured floor, the credential is disabled automatically so the remaining quota stays available for your direct use (e.g. the account signed into your desktop client); it is re-enabled automatically once the window recovers.

Quota readings are fetched live from upstream and keep working while a credential is disabled — regular resets, manual reset cards, and official grants are all detected immediately instead of waiting on a cached reset time.

## Problem it solves

In a multi-account Codex pool, one account is often signed into your desktop client. The proxy's rotation drains that account too, and once it bottoms out the client locks the composer locally with a "usage limit reached" gate — even when your requests actually go through other proxy accounts. This plugin keeps a configurable slice of each designated account's quota out of the proxy's reach — the per-account quota reserve floor the CLIProxyAPI maintainers left for external implementations (issue #6303).

## Features

- **Auto-discovery** of all local Codex OAuth credentials; the management page shows live remaining quota, reset countdowns and guard state per account
- **Quota floor**: disable at the floor, re-enable on recovery; early resets (reset cards / official grants) detected immediately
- **Anti-abuse query strategy**: passive quota signals from real traffic response headers (zero extra upstream requests) when available; fallback active queries only on an interval for unprotected credentials; clock-aligned query slots
- **Visual settings**: change floors, pick accounts from a dropdown of discovered credentials, tune query cadence — saved and applied live, no config file editing or restarts
- **Ownership safety**: only credentials disabled by the guard are re-enabled by it; manual disables are never touched; ownership survives restarts
- **Event log**: every disable/re-enable action recorded and visible on the page

## Install

**Option A: management panel / EasyCLIProxyAPI plugin page** (recommended)

Install `codex-quota-guard` from the plugin store or management panel.

**Option B: manual**

1. Download the zip for your platform from [Releases](https://github.com/HanShuheng/cpa-plugin-quota-guard/releases) and unpack `codex-quota-guard-v<version>.dylib` (macOS) / `.so` (Linux)
2. Place it under `plugins/<os>/<arch>/`, e.g. `plugins/darwin/arm64/`
3. Add the config below to `config.yaml` and restart CPA

## Configuration

```yaml
plugins:
  configs:
    codex-quota-guard:
      enabled: true
      poll-interval-seconds: 300          # slot interval (clock-aligned)
      primary-min-remaining: 10           # floor: 5-hour window remaining %
      secondary-min-remaining: 5          # floor: weekly window remaining %
      passive-skip-seconds: 300           # skip active query when passive data is this fresh
      unprotected-fetch-interval-seconds: 1800  # fallback active-query interval for unprotected credentials
      accounts:                           # only listed credentials are guarded (none by default)
        - match: "you@example.com"        # file-name / label / email substring
          # primary-min-remaining: 15     # optional per-account overrides
          # secondary-min-remaining: 8
      # protect-all-codex: true           # or guard every Codex OAuth credential
      # exclude: ["backup@example.com"]   # exclusions for protect-all
```

Everything can also be edited in the plugin page's Settings card and applies within seconds.

| Option | Default | Meaning |
|---|---|---|
| `poll-interval-seconds` | 300 | Query slot interval; firings align to multiples of it |
| `primary-min-remaining` | 10 | 5-hour window floor in % remaining; 0 disables the floor for that window |
| `secondary-min-remaining` | 5 | Weekly window floor in % remaining; 0 disables |
| `accounts[].match` | — | Substring matched against file name / label / email |
| `accounts[].primary/secondary-min-remaining` | global | Per-account floor overrides |
| `protect-all-codex` | false | Guard every Codex OAuth credential |
| `exclude[]` | — | Credential substrings to exclude (with protect-all) |
| `passive-skip-seconds` | 300 | Skip the active query when passive data arrived within this many seconds |
| `unprotected-fetch-interval-seconds` | 1800 | Fallback active-query interval for unprotected credentials (status display only) |

## How it works

1. **Discover**: each slot enumerates Codex OAuth credentials via host `host.auth.list`.
2. **Read quota (three-layer anti-abuse strategy)**:
   - **Passive first**: with the `usage_plugin` capability, the host forwards quota signals from real traffic response headers (`X-Codex-*-Used-Percent` etc.); credentials with real traffic get readings for free
   - **Differentiated active queries**: protected credentials are actively queried each slot when no passive data exists (guard decisions depend on it); unprotected ones only on the fallback interval
   - **Clock alignment**: active queries fire on clock boundaries
3. **Decide**: at/below floor → disable via `host.auth.save` (equivalent to disabling in the panel) and record the breached windows; all guarded windows above floor → re-enable.
4. **Fallback**: if upstream queries keep failing, recovery falls back to the reset times recorded at disable time, so a usable credential is never pinned off.
5. **Ownership**: state persists in `plugins/data/codex-quota-guard/state.json`; manually disabled credentials are never touched.

## Management API & page

- `GET /v0/management/codex-quota-guard/status` — discovered credentials, live quota, data source (passive/cached/active), guard state, events
- `POST /v0/management/codex-quota-guard/check` — run one pass now
- Management page (panel sidebar → plugin pages → Codex Quota Guard, or `GET /v0/resource/plugins/codex-quota-guard/page`): account cards, visual settings, event log

## Build & development

Requires Go ≥ 1.26. Uses CLIProxyAPI's stable C ABI (`sdk/pluginabi`) — not coupled to the host binary's Go version, so CPA upgrades normally don't require a rebuild (except ABI changes).

```sh
sh scripts/build.sh 0.6.1        # current platform
go test ./...                    # unit tests
```

Push a `v*` tag and CI builds all four platforms and publishes the release (`.github/workflows/release.yaml`).

### Extension guide

- **Layout**: `main.go` (C ABI bridge, rarely touched) · `guard.go` (lifecycle, config, host RPC, management API) · `tick.go` (per-pass read & decide) · `passive.go` (usage.handle passive quota signals) · `web/page.html` (embedded management page, single file with HTML/CSS/JS via `go:embed`)
- **New host callbacks**: add wrappers over `call()` in `guard.go`; method names are the constants in `sdk/pluginabi` (`host.auth.*` / `host.http.*` / `host.log`, …)
- **New config options**: add a field to `Config` (yaml+json tags) → declare it in `registration()` ConfigFields → mirror in the page's `fillSettings`/`collectSettings`
- **New page sections**: `web/page.html` is a single HTML/CSS/JS file; always `node --check` the extracted `<script>` after edits before building

## License

MIT
