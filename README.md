# Codex Quota Guard（CPA 插件）

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 原生插件：为 Codex（ChatGPT）OAuth 凭证设置**额度保底**。当某条凭证的 5 小时窗口或周窗口剩余额度逼近设定下限时，自动把该凭证禁用，把剩余额度留给你自己直连使用（比如桌面端登录的账号）；窗口重置后自动恢复。额度数据直接向上游实时查询，凭证处于禁用状态也能探测——手动重置、官方补偿重置都会被立刻发现。

## 解决什么问题

多账号 Codex 池里，经常有一条账号是你桌面端/手机端登录的主账号。CPA 的轮换策略会把它的额度一起榨干，一旦触底，客户端本地就会弹出"达到用量上限"并锁住发送按钮（即使请求走的是 CPA 的其他账号）。这个插件让主账号**永远留一部分额度不被代理消耗**，对应 [CLIProxyAPI #6303](https://github.com/router-for-me/CLIProxyAPI/issues/6303) 提出而官方留给外部实现的"per-account quota reserve floor"。

## 配置

```yaml
plugins:
  configs:
    codex-quota-guard:
      enabled: true
      poll-interval-seconds: 300      # 轮询间隔（秒）
      primary-min-remaining: 10       # 全局默认：5 小时窗口剩余 % 下限
      secondary-min-remaining: 5      # 全局默认：周窗口剩余 % 下限
      accounts:                       # 只保护列出的凭证；默认不保护任何账号
        - match: "you@example.com"    # 凭证文件名 / 标签 / 邮箱的子串
          # primary-min-remaining: 15   # 可选：该账号单独覆盖
          # secondary-min-remaining: 8
      # protect-all-codex: true       # 或保护全部 Codex OAuth 凭证
      # exclude: ["backup@example.com"]  # 配合 protect-all 排除个别账号
```

规则：

- **默认不保护任何账号**——装上插件不改变任何行为，配置了才生效。
- 剩余 ≤ 下限 → 通过 `host.auth.save` 禁用该凭证（与管理面板禁用等效）；所有受保护窗口恢复到下限之上 → 自动重新启用。
- 窗口下限设为 `0` 或负数 = 对该窗口不设限。
- 你**手动**禁用的凭证，插件绝不碰；只有插件自己禁用的才会被插件恢复。重启 CPA 后归属关系不丢失（状态持久化在 `plugins/data/codex-quota-guard/state.json`）。

## 工作原理

1. 每 `poll-interval-seconds` 调用 `host.auth.list` 发现全部 Codex OAuth 凭证。
2. 用每条凭证自己的 token 调 `GET https://chatgpt.com/backend-api/wham/usage`（经 CPA 的 `host.http.do`，走全局代理配置），读取两个窗口的实时剩余百分比与重置时间。**禁用状态也能查**，所以额度提前恢复（重置卡/官方发放）会被立即发现。
3. 触底的凭证被禁用并记录触底窗口的重置时间；若上游查询长时间失败，退化为按重置时间恢复，保证不会把可用的凭证一直关着。
4. 状态与最近观测可通过管理 API 查看：
   - `GET /v0/management/codex-quota-guard/status` — 全部已发现凭证、实时额度、保护状态
   - `POST /v0/management/codex-quota-guard/check` — 立即执行一轮检查

所有动作写入 CPA 日志（关键字 `codex-quota-guard`）。

## 安装

1. 把对应平台的 `codex-quota-guard-v<版本>.dylib`（macOS）/ `.so`（Linux）放入 `plugins/<os>/<arch>/`，例如 `plugins/darwin/arm64/`。
2. 在 `config.yaml` 中加入上面的 `plugins.configs.codex-quota-guard` 配置。
3. 重启 CPA。

或通过 EasyCLIProxyAPI / 管理面板安装发布包。

## 构建

```sh
sh scripts/build.sh 0.1.0          # 当前平台
sh scripts/ci-build.sh             # 全平台（Docker）
```

依赖：Go ≥ 1.26。插件走 CPA 的稳定 C ABI（`sdk/pluginabi`），不依赖宿主二进制的 Go 版本，CPA 升级后无需重新编译（ABI 变更除外）。

## 与 Lamplighter 的区别

[Lamplighter](https://github.com/PosvdM/cpa-plugin-lamplighter) 监控额度并**通知**你；本插件在额度触底时**直接行动**（禁用/恢复凭证），两者可以共存。

## License

MIT
