# commandcode-go — CLIProxyAPI 插件

把 **CommandCode 订阅**（`/alpha/generate` 私有 CLI 协议）接入 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)。装好后，任何 CPA 支持的客户端（OpenAI Chat Completions、Claude Messages、Codex Responses 等）都能直接使用你的 CommandCode 订阅额度跑 DeepSeek / GLM / Kimi / MiniMax / Qwen 等模型。

协议转换逻辑移植自 [thaolaptrinh/commandcode-api-proxy](https://github.com/thaolaptrinh/commandcode-api-proxy)（独立 Node 代理），本项目是它的 CPA 原生插件形态：进程内动态库、复用宿主的凭证池、调度、代理、日志与协议翻译，不再需要单独跑一个 8787 端口的代理进程。

## 与商店 `commandcode` 插件的区别

| | `commandcode`（ahoo） | **`commandcode-go`（本插件）** |
|---|---|---|
| 上游端点 | `/provider/v1/chat/completions` | `/alpha/generate` |
| 需要的订阅 | Provider 付费层 | 标准订阅（含 Go） |
| 形态 | API key 直连 | CLI 指纹头 + 事件流翻译 |

插件 ID 不同，两者可以共存安装。

## 前提

- CLIProxyAPI 需要是支持动态库插件的 CGO 构建：`curl -sI http://localhost:8317/v0/management/plugins | grep X-CPA-SUPPORT-PLUGIN` 显示 `1`。
- macOS / Linux / Windows（amd64 / arm64）。

## 构建安装

```bash
make build
make install PLUGINS_DIR=/path/to/cliproxyapi/plugins
```

发布产物（四平台 zip + checksums，符合插件商店格式）：

```bash
make package-darwin-arm64 package-darwin-amd64 package-linux-amd64 package-linux-arm64
```

## 配置

`config.yaml`：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    commandcode-go:
      enabled: true
      # api_base: "https://api.commandcode.ai"  # 可选：改上游地址
      # cli_version: "0.40.3"                   # 可选：钉死 CLI 版本（默认自动从 npm 刷新，24h 缓存）
      # idle_timeout_ms: 120000                 # 可选：流式块间超时，0 关闭
      # max_retries: 2                          # 可选：流开始前的上游重试次数
      # project_slug: "my-project"              # 可选：x-project-slug 头
```

## 凭证

API key 从 https://commandcode.ai/settings 获取，写入宿主 auth 目录（默认 `~/.cli-proxy-api`）：

```bash
cat > ~/.cli-proxy-api/commandcode-go-main.json <<'JSON'
{"type":"commandcode-go","api_key":"<你的 key>","label":"main"}
JSON
```

重启 CPA 即生效。支持多文件多 key（宿主负责轮转与失败切换）；每个 auth 文件可用 `"base_url"` 字段单独覆盖上游地址。无交互式登录：`auth.login.*` 明确返回不支持。

## 使用

模型 ID 以 `GET /v1/models` 为准（完整 ID，如 `deepseek/deepseek-v4-pro`）。别名（`deepseek-v4-pro`、`glm5.2` 等）由插件内部解析，但**客户端必须请求 CPA 已注册的完整 ID**，否则宿主机由不会把它路由到本插件。

```bash
curl -s http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <cpa-key>" -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-pro","messages":[{"role":"user","content":"say hi"}],"stream":true}'
```

Claude Code / Codex / 任意 OpenAI 兼容客户端按 CPA 常规方式接即可；Anthropic 流/非流均由插件原生产出（见「行为说明」）。

## 行为说明

- **上游恒为流式**：非流式请求由插件聚合 CC 事件后返回完整响应（OpenAI `chat.completion` / Anthropic message）。
- **Anthropic 原生输出**：宿主没有注册 chat-completions→anthropic 的**流式** transformer，插件因此声明 `anthropic` 输出格式并为 Claude 客户端直接产出原生 SSE 事件（`message_start`/`content_block_*`/`message_delta`/`message_stop`，thinking 用 `thinking_delta`、工具用 `input_json_delta`、usage 按 message_start/message_delta 拆分）。
- **异步流泵**：`executor.execute_stream` 打开上游（同步，保留流前错误的 HTTP 状态语义）后立即返回，chunk 在后台 goroutine 经 `host.stream.emit` 推送——宿主 stream bridge 队列仅 16，在 RPC 内同步泵会在长流上死锁。
- **推理链**：`reasoning-delta` → OpenAI `reasoning_content` / Anthropic thinking block。
- **会话亲和**：客户端带回话头（`X-Session-Id`、`X-Claude-Code-Session-Id`、`Conversation-Id` 等）时复用作上游 `x-session-id`，同一会话跨轮次保持连续并利于上游缓存；缺失或非法时回退新 UUID。
- **并行工具调用**：按 `toolCallId` 分配稳定流式 index，`tool-call` 终态与增量做一致性合并（含 id/name 未知的缓冲兜底）。
- **reliability**：5xx/429/传输错误仅在流开始前重试（线性退避）；块间 idle 超时合成 finish；上游 error 事件以合法流形态送达客户端；错误信息脱敏 API key；客户端断连立即关闭上游流、停止烧额度。
- **凭证安全**：key 只从执行请求的 `StorageJSON` 即用即弃，不复制进宿主属性；日志不输出 key 或原始凭证。
- **CLI 指纹**：`User-Agent: commandcode-cli/<ver>`、`x-command-code-version` 等头缺一不可（上游会拒绝疑似代理流量），版本号自动从 npm 刷新。
- 与参考实现一致，本插件默认面向**本机/受信网络**部署；`/alpha/generate` 不是公开 API，请自行评估暴露风险。

## 验证状态

真实订阅 key 端到端验收已通过（2026-10-28，CPA 源码 v7.2.141 沙箱实例，以及生产 brew v8.0.40 实例）：OpenAI Chat Completions、Claude Messages、Codex Responses 三个 surface 的流式与非流式、thinking 块、工具调用流、usage 明细、断连清理均正常。

## 开发

```
main.go            CGO ABI 骨架（cliproxy_plugin_init/call/free/shutdown）
hostbridge.go      宿主回调封装（host.http.*、host.stream.*、host.log）
rpc.go             JSON envelope 与线上传输类型
register.go        能力声明与 RPC 路由
config.go          插件自有 YAML 配置（标量行扫描，零依赖）
auth.go / storage.go   API-key 凭证文件 ⇄ AuthData
catalog.go / catalog.json  静态模型目录 + 别名 + effort 表（可被 provider API 动态合并）
models.go          model.static / model.for_auth
upstream.go        /alpha/generate 请求、指纹头、重试、NDJSON 流泵
events.go          事件行解析（扁平/嵌套双形态）
translate_request.go   chat-completions → CC 请求体
translate_stream.go    CC 事件 → chat.completion.chunk / 非流式聚合
translate_claude.go    CC 事件 → Anthropic 原生 SSE / 非流式 message（Claude 面）
executor.go        executor.execute / execute_stream / count_tokens
version.go         npm CLI 版本刷新
```

```bash
go test ./...   # 转换层、目录、凭证、RPC 单测
```

## License

MIT
