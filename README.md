# Codex Quota Guard（CPA 插件）

[English](README_EN.md) | 中文

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 原生插件：为 Codex（ChatGPT）OAuth 凭证设置**额度保底**。当某条凭证的 5 小时窗口或周窗口剩余额度逼近设定下限时，自动禁用该凭证，把剩余额度留给你自己直连使用（比如桌面端/手机端登录的账号）；窗口重置后自动恢复启用。

额度数据直连上游实时查询，凭证处于禁用状态也能探测——正常重置、手动重置卡、官方补偿发放都会被立即发现，不会傻等缓存的重置时间。

## 解决什么问题

多账号 Codex 池里，经常有一条账号是你桌面端登录的主账号。代理的轮换策略会把它的额度一起榨干，一旦触底，客户端本地就会弹出"达到用量上限"并锁住发送（即使请求走的是代理的其他账号）。这个插件让指定账号**永远留一部分额度不被代理消耗**，对应 CLIProxyAPI 官方留给外部实现的 per-account quota reserve floor（issue #6303）。

## 特性

- **自动发现**本地全部 Codex OAuth 凭证，管理页面实时展示每个账号两个窗口的剩余额度、重置倒计时与守卫状态
- **额度保底**：剩余降到下限自动禁用，恢复后自动启用；提前重置（重置卡/官方发放）即时探测
- **防频繁设计**：账号刚服务过真实请求时直接采用响应头自带的额度信号（被动数据，零额外请求）；未保护账号仅按兜底间隔主动查询；主动查询槽位对齐整点
- **可视化设置**：管理页面直接改阈值、选账号（下拉列出已发现凭证）、调查询节奏，保存即热生效，无需改配置文件或重启
- **归属安全**：只恢复自己禁用的凭证，手动禁用的绝不触碰；重启后归属关系不丢失
- **事件日志**：每次禁用/恢复动作记录在案，页面可查

## 安装

**方式一：管理面板 / EasyCLIProxyAPI 插件页**（推荐）

在插件商店或管理面板中安装 `codex-quota-guard`。

**方式二：手动**

1. 从 [Releases](https://github.com/HanShuheng/cpa-plugin-quota-guard/releases) 下载对应平台的 zip，解压出 `codex-quota-guard-v<版本>.dylib`（macOS）/ `.so`（Linux）
2. 放入 `plugins/<os>/<arch>/`，例如 `plugins/darwin/arm64/`
3. 在 `config.yaml` 加入配置（见下），重启 CPA

## 配置

```yaml
plugins:
  configs:
    codex-quota-guard:
      enabled: true
      poll-interval-seconds: 300          # 查询槽位间隔（对齐整点）
      primary-min-remaining: 10           # 全局下限：5 小时窗口剩余 %
      secondary-min-remaining: 5          # 全局下限：周窗口剩余 %
      passive-skip-seconds: 300           # 最近被动数据在此时限内则跳过主动查询
      unprotected-fetch-interval-seconds: 1800  # 未保护凭证的主动查询兜底间隔
      accounts:                           # 只保护列出的凭证；默认不保护任何账号
        - match: "you@example.com"        # 凭证文件名/邮箱子串
          # primary-min-remaining: 15     # 可选：该账号单独覆盖
          # secondary-min-remaining: 8
      # protect-all-codex: true           # 或保护全部 Codex OAuth 凭证
      # exclude: ["backup@example.com"]   # 配合 protect-all 排除个别账号
```

所有配置都可以在插件管理页面的"设置"卡片中可视化修改，保存后几秒内生效。

### 配置项说明

| 配置项 | 默认 | 说明 |
|---|---|---|
| `poll-interval-seconds` | 300 | 查询槽位间隔；实际触发时刻对齐到该间隔的整倍数边界 |
| `primary-min-remaining` | 10 | 5 小时窗口剩余百分比下限，达到即禁用；0 = 该窗口不设限 |
| `secondary-min-remaining` | 5 | 周窗口剩余百分比下限，达到即禁用；0 = 该窗口不设限 |
| `accounts[].match` | — | 受保护凭证的匹配子串（文件名 / 标签 / 邮箱任一包含即命中） |
| `accounts[].primary/secondary-min-remaining` | 全局值 | 该账号单独覆盖的下限 |
| `protect-all-codex` | false | 保护全部 Codex OAuth 凭证 |
| `exclude[]` | — | 配合 protect-all 排除的凭证子串 |
| `passive-skip-seconds` | 300 | 凭证最近一次真实请求（被动数据）在此时限内时跳过主动查询 |
| `unprotected-fetch-interval-seconds` | 1800 | 未保护凭证的主动查询兜底间隔（仅用于状态展示） |

## 工作原理

1. **发现**：每个槽位调用宿主 `host.auth.list` 枚举全部 Codex OAuth 凭证。
2. **取数（防频繁三层策略）**：
   - **被动优先**：注册 `usage_plugin` 能力后，宿主把每次真实请求响应头中的额度信号（`X-Codex-*-Used-Percent` 等）推送给插件；凭证有真实流量时读数免费获得，零额外请求
   - **差异化主动查询**：受保护凭证在无被动数据时每槽位主动查询（守卫决策依赖）；未保护凭证仅在兜底间隔到期时查一次用于展示
   - **整点对齐**：主动查询固定在时钟边界，流量模式规律自然
3. **决策**：剩余 ≤ 下限 → 通过 `host.auth.save` 禁用该凭证（与管理面板禁用等效）并记录触底窗口；所有受保护窗口恢复到下限之上 → 自动重新启用。
4. **兜底**：上游查询长时间失败时，退化为按禁用时记录的重置时间恢复，保证不会把可用的凭证一直关着。
5. **归属**：状态持久化在 `plugins/data/codex-quota-guard/state.json`；手动禁用的凭证绝不触碰。

## 管理 API 与页面

- `GET /v0/management/codex-quota-guard/status` — 全部已发现凭证、实时额度、数据来源（passive/cached/active）、守卫状态与事件
- `POST /v0/management/codex-quota-guard/check` — 立即执行一轮检查
- 管理页面（管理面板侧边栏"插件页面 → Codex Quota Guard"，或 `GET /v0/resource/plugins/codex-quota-guard/page`）：账号卡片、可视化设置、事件日志

## 构建与开发

依赖 Go ≥ 1.26，使用 CLIProxyAPI 的稳定 C ABI（`sdk/pluginabi`），**不与宿主二进制的 Go 版本耦合**，CPA 升级后通常无需重新编译（ABI 变更除外）。

```sh
sh scripts/build.sh 0.6.1        # 当前平台
go test ./...                    # 单元测试
```

推 `v*` 标签时 CI 自动构建四平台产物并发布 Release（`.github/workflows/release.yaml`）。

### 扩展指引

- **目录结构**：`main.go`（C ABI 桥接，基本不用动）· `guard.go`（生命周期、配置、宿主 RPC、管理 API）· `tick.go`（每轮检查的取数与决策）· `passive.go`（usage.handle 被动额度信号）· `web/page.html`（内嵌管理页面，`go:embed` 打进二进制）
- **新增宿主回调**：在 `guard.go` 的 `call()` 上加包装方法，方法名见 `sdk/pluginabi` 常量（`host.auth.*` / `host.http.*` / `host.log` 等）
- **新增配置项**：`Config` 结构体加字段（yaml+json tag）→ `registration()` 的 ConfigFields 声明 → 页面 `fillSettings`/`collectSettings` 同步
- **新增页面区块**：`web/page.html` 单文件包含 HTML/CSS/JS，改动后务必 `node --check` 校验 `<script>` 内容再编译

## License

MIT
