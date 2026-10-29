# TASKS.md

## Now（当前推进，最多 3 条）
- （暂无）

## Next（近期候选，最多 10 条）
- [ ] 会话亲和的生产端到端验证（同一会话头连发两次请求，确认上游 x-session-id 复用）
  - status: todo
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md
- [ ] （可选）安装 agent-browser 后做资源页视觉验证
  - status: todo
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md
- [ ] （可选）向 CLIProxyAPI-Plugins-Store 提交 registry PR（v0.2.0 已发布）
  - status: todo
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md

## Blocked（等待输入或外部条件）
- （暂无）

## Recently Done（最近完成，最多 5-10 条）
- [x] v0.2.1：凭证改为单 key 二选一（CLI 登录 key 兼顾推理+额度；settings 的 API key 仅推理）。实测两种 key 均可推理，生产已切换为 CLI 登录 key 并验证额度与推理
- [x] v0.2.0：管理资源页 + 额度能力（`management_api` + `quota_provider`）。额度经 CLI billing 端点取实时 5h/周窗口，429 与本地计数兜底；生产验证 `supports_quota: true`、额度与页面均正常
- [x] 逆向 command-code CLI（v1.88.2）定位额度渠道：`/alpha/billing/credits`、`/alpha/billing/subscriptions`、`/alpha/usage/summary`、`/alpha/whoami`；确认 billing 端点只认 CLI 登录 key
- [x] 发布 v0.1.3：public 仓库 Goodwu/cpa-plugin-commandcode，tag 触发 release，四平台 zip + checksums 产物齐全，CI/Release 全绿
- [x] 会话亲和（v0.1.2）：客户端会话头（X-Session-Id / X-Claude-Code-Session-Id / Conversation-Id 等）复用为上游 x-session-id，非法值回退 UUID；单测覆盖
- [x] 生产清理与补验：model-router 加 `data_path` 后恢复注册（5 插件全绿）、移除 example 配置残留、Codex/Responses 流式面验证通过、darwin 打包格式验证通过
- [x] 生产内容级验收通过（额度重置后）：`cc/deepseek/deepseek-v4-flash` 非流式返回真实内容 + usage；Claude 流式完整事件序列 + 文本 + end_turn
- [x] 生产部署到 brew cliproxyapi v8.0.40：dylib + plugins.configs + auth（prefix cc），服务重启，registered/effective=true，69 个 cc/ 模型上线
- [x] 生产差异修复（v0.1.1）：force-model-prefix 前缀剥离（403 Model/provider not recognized）；上游 4xx 为 brotli 错误体导致乱码（不再声明 Accept-Encoding + 非文本体 hex 预览）
- [x] 真实 key 端到端验收：OpenAI/Claude 双流式、非流式、thinking、工具调用、usage 明细、断连清理全部通过
- [x] 实现 commandcode-go CLIProxyAPI 插件：/alpha/generate 反代（auth/model/executor 三能力、chat-completions 翻译、宿主 HTTP 桥流泵、CLI 指纹、重试/超时/脱敏、catalog 动态合并）
- [x] 工程链路：Makefile 四平台矩阵、package-release 脚本、CI/release workflow、README

## 规则
- 新任务必须写入本文件
- 完成任务必须打勾
- 建议任务状态：todo / in_progress / blocked / done
- 活跃任务建议填写 `context: archives/conversations/<topic>.md`
- 按需从本文件定位任务、读取有效 context，并在需要时检索 active memory
- 旧完成项超过上限时，压缩进对应 conversation 后从本文件移除
- 每次推进使用 Git 提交信息记录变更
