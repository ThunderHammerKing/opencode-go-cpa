## opencode-go-cpa v0.1.0

首个版本。

- OpenCode Go 接入为单一 provider（`opencode-go/<model>`），一个 key 池同时服务 chat-completions / messages / responses 三种上游端点
- 凭证在「认证文件 → 登录」里添加（粘贴 API key），配额页按本地记账展示 5h / 每周 / 每月用量
- 设置全部可在 管理面板 → 插件管理 → 编辑配置 表单里完成
- 遵循官方客户端指引：自报 UA + 稳定 `x-opencode-session`，保留 Codex / Claude Code 原生会话头
