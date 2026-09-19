# opencode-go-cpa

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 OpenCode Go 插件：把 [OpenCode Go](https://opencode.ai/docs/go/) 订阅接入成一个 provider（provider key `opencode-go`，模型前缀 `opencode-go/<model>`），一个 key 池同时服务上游三种端点（`chat/completions`、`messages`、`responses`），凭证与配额全部在管理面板图形化操作。

> 参考了 [massiveits/opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) 的实现并按需求重构，主要差异见文末。

## 安装

方式一（插件商店）：把本仓库的 registry 地址加进 CPA 配置，然后在 管理面板 → 插件商店 安装：

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/ThunderHammerKing/opencode-go-cpa/main/registry.json"
```

方式二（手动）：从 Releases 下载对应平台的压缩包，把动态库放进插件目录（macOS ARM 为 `plugins/darwin/arm64/opencode-go-cpa-v0.1.2.dylib`），并在配置里启用：

```yaml
plugins:
  enabled: true
  configs:
    opencode-go-cpa:
      enabled: true
```

## 添加凭证

到 **认证文件 → 登录** 选择 OpenCode Go：插件会打开一个粘贴 API key 的页面，把 [opencode.ai/auth](https://opencode.ai/auth) 里复制来的 key 粘进去保存即可。OpenCode Go 没有 OAuth，只有 API key。

凭证保存在 CPA 的 auth 文件里，享受统一的调度、轮换、冷却与优先级管理；登录时粘贴的 key 会被自动用于模型目录发现。

## 设置

全部设置都在 管理面板 → 插件管理 → 编辑配置 的表单里（也可以直接改 `plugins.configs.opencode-go-cpa`）：

| 键 | 说明 | 默认 |
|---|---|---|
| `base-url` | 上游地址 | `https://opencode.ai/zen/go/v1` |
| `model-prefix` | 模型前缀，留空则不加前缀 | `opencode-go` |
| `refresh-interval` | 模型目录刷新周期（≥1m） | `15m` |
| `request-timeout` | 上游请求超时 | `5m` |
| `max-concurrent-per-key` | 单凭证并发上限，0 不限 | `4` |
| `user-agent` | 上游 User-Agent | `opencode-go-cpa/0.1.0` |
| `forward-session` | 从客户端原生会话头派生并转发 `x-opencode-session` | `true` |
| `cooldown-on-429` | 上游 429 时在配额页标记限流 | `true` |
| `respect-retry-after` | 遵循上游 Retry-After | `true` |
| `api-keys` | 可选的种子 key 列表 | 空 |

高级项（仅 YAML）：`catalog-url`、`route-overrides`（按模型覆盖上游端点）、`stale-while-unavailable`、`allow-http`、`max-response-bytes`。

## 配额页

侧边栏 **OpenCode Go Quota**：按凭证展示 5 小时 / 每周 / 每月三个窗口的美元用量与占比。数据是**本地记账**（token × 官方价目表，价目内置），不是上游权威数字；上游 429 会把对应窗口标红并显示重置时间。

## 与官方 opencode-go-cliproxyapi 插件的差异

- **客户端协议由 CPA 翻译**：本插件只声明 chat-completions，Claude Code / Codex 的原生请求由 CPA 内置翻译器转换，插件专注上游三协议路由；省掉约六成协议转换代码。
- **凭证走 auth 文件 + 可用的登录流程**：登录按钮打开贴 key 页面并落成 auth 记录（原插件的登录是个必失败的桩，面板上是个死按钮）。
- **错误必带 http_status**：不再出现 status=0 的错误导致 CPA 误冷却整个 provider（原插件 issue #2）。
- **`developer` 角色规范化**为 `system`，DeepSeek 系模型不再 400（原插件 issue #5）。
- **遵循官方客户端指引**：用自己的 UA（`opencode-go-cpa/x.y`）+ 稳定的 `x-opencode-session`，并保留 Codex / Claude Code 原生会话头；不冒充官方 CLI。
- **配额是本地记账页**，附带价目表与 429 窗口标记；不代理未公开的上游接口。

## 卸载 / 回滚

管理面板 → 插件管理 → 删除；或删掉插件目录里的 `opencode-go-cpa-*.dylib` 并移除 `plugins.configs.opencode-go-cpa`。auth 文件里的凭证不受影响。

## 构建

```bash
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o opencode-go-cpa.dylib .
rm -f opencode-go-cpa.h
```

推 tag（`v0.1.0`）后 GitHub Actions 会产出全平台压缩包与校验文件。
