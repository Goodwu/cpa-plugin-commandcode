# TASKS.md

## Now（当前推进，最多 3 条）
- [ ] quota-hub 全链路实施（阶段1-5）：独立额度守护进程 + ZCode 原生弹层，最终撤销旧注入链路
  - status: in_progress
  - context: archives/conversations/zcode-quota-hub-design.md
  - 架构决策（2026-11-03，与用户确认）：quota-hub 不实现为 CPA 插件，改为独立守护进程（Go 二进制 + LaunchAgent，仓库 ~/src/quota-hub，运行时目录 ~/.quota-hub）。与 CPA 唯一交互是本机管理 API；模型→账号映射用 auth-files ⊕ 逐账号 auth-files/models（与 host.auth.models 同源，全局 registry）。消除 dylib ABI 耦合、宿主内死锁/重入风险、validPlugin 假能力 hack、改生产 CPA 配置的需求
  - 进展（2026-11-03）：阶段2 代码完成（daemon 全功能+race 全绿+无密钥降级实测）；阶段3 隔离副本验证通过（三层注入 31,826 文件比对、UI 无回归、CDP 主世界 window.quotaHub 与原生桥并存、capabilities/query/非法参数全通过、daemon 收到真实 query、a2t 捕获当前模型）；两次提交于 ~/src/quota-hub
  - 下一步：用户提供管理密钥（写入 ~/.quota-hub/management.key，0600）→ quota-hub -poc（阶段1）→ make install 部署+curl 真实额度验收（阶段2）→ 弹层真实数据视觉验证 → 可信 3.17.2 原件 → 正式应用补丁（阶段4）→ 验收并撤销 ZCODE_BASE_URL + com.cc-quota-proxy（阶段5）
  - 阻塞点：管理密钥明文、阶段4可信原件来源、阶段5停旧链路确认

## Next（近期候选，最多 10 条）
- [ ] 修复存量测试竞态与缓存污染（独立于锁定过滤）：version 后台 npm 查询 goroutine 与测试替换 lookupCLIVersion 的 data race（触发入口不止 TestCLIVersionPinning，quota_test fetchQuotaFromBilling→currentCLIVersion 同样可触发，-race 单轮即可能失败）；TestQuotaCacheTTL 在 -count>1 下同进程 quotaCache 污染
  - status: todo
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md
- [ ] 会话亲和的生产端到端验证（同一会话头连发两次请求，确认上游 x-session-id 复用）
  - status: todo
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md
- [ ] （可选）向 CLIProxyAPI-Plugins-Store 提交 registry PR（v0.2.0 已发布）
  - status: todo
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md

## Blocked（等待输入或外部条件）
- （暂无）

## Recently Done（最近完成，最多 5-10 条）
- [x] 第二轮复审M1残留修复并部署0.2.7：过滤改为使用点即时读开关（catalogIDs/modelLockReason/lockedBareNameReason），热重配关闭后已发布锁定快照立即失效（无需刷新）、在途刷新发布锁定集在关闭期间惰性、重开恢复最近已知锁定态；裸名撞名确定性报告可解锁最低档；README补开关即时语义与null门槛例外；热重配/在途刷新/重开/撞名/并发race测试通过，生产列表与0.2.6一致、推理正常
- [x] critical review 修复（M1-M3/m1-m4）并部署0.2.6：开关关闭跳过 billing 探测且列表不过滤；未知 minimumPlan 按CLI链回退内嵌目录快照（从cli.mjs提取106条gating入catalog.json）再回退类别；订阅仅 active/trialing/past_due 参与门控；锁定裸名末段匹配拦截；lockedPlan 改存判定原因（类别门槛premium→Pro）；测试 pin cli_version+唯一key消除新增race入口；README 收窄TTL懒刷新/开关/错误码措辞。生产0.2.6验证列表与0.2.5一致、锁定仍过滤、推理正常；vet/test/race(-count=1)与reviewer指定组合通过
- [x] 按订阅套餐过滤锁定模型并部署0.2.5：目录改读 /provider/v1/models?view=client（含 category/minimumPlan），按 CLI 1.88.2 evaluateModelAccess 规则（套餐等级+类别+credits 全解锁+降级不锁）从模型列表剔除锁定模型并在执行器侧 403 model_not_in_plan 拦截；filter_locked_models 可关。生产验证 Go 套餐下 9 个锁定模型（grok-4.6/4.7、Qwen3.8-Max-Prime、muse-* 等）从 /v1/models 消失、新增 7 个 client view 可用模型、流式/Anthropic/非流式推理与锁定请求拒绝均正常；vet/test/race/build 全绿
- [x] 按要求整体删除独立状态页、侧栏入口及诊断接口，部署0.2.4；浏览器确认入口消失、旧公共地址404、统一额度刷新正常，vet/test/race/build通过
- [x] 统一5小时/每周/月度额度文案并移除月度分隔线；补GOAT和两类Pro分母回归，明确静态CLI套餐表来源与非Go真实验证缺口；前端检查通过并部署
- [x] 部署月度文案微调：隐藏重复Monthly credits分组标题，中文月度额度标签；静态渲染确认标题消失、中文及22.7%保留，前端完整检查通过
- [x] 部署0.2.3：状态页不再误用混淆密钥并停止认证失败自动重试；离线复现旧版7次失败与新版0次，真实状态页停留75秒后配额刷新成功，无再次封禁
- [x] 补齐月度订阅比例：按CLI已知套餐分配，未知/无效账期不猜测；生产卡片显示22.7%剩余及11/20重置，race/vet/test/build与前端检查通过
- [x] 生产统一配额页面验收：全部4个配额凭证、CommandCode分类仅1个且在分类栏下统一网格；真实5h/周额度、单卡刷新及时间线通过
- [x] 移出被CPA递归扫描的验证备份，浏览器确认5个正式凭证且CommandCode仅1个；隔离构建并部署统一配额页面
- [x] 本地通用插件额度接入统一机制，移除独立区域；Critical Reviewer 复审通过，431项测试及模拟浏览器跨页面验证通过
- [x] 清除管理中心临时IP封禁，页面既有密钥登录成功；本地修复状态页401/403无限重试与混淆值误用
- [x] 部署 0.2.2 并验收：生产五插件正常、实时额度、非流式正文、Claude 双工具流；隔离 v8.0.40 宿主验证会话亲和、错误脱敏/失败统计、长流卸载及重新加载推理；已有版本和配置可回滚
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md
- [x] critical review 修复：Claude 工具流/首帧、错误脱敏与失败统计、shutdown 生命周期、工具禁用、DONE、版本刷新、assistant 历史及额度窗口；补回归测试，race/vet/本机动态库构建通过；critical reviewer 独立复审通过（并发额度及生命周期重点测试 race 连跑 10 次）
  - context: archives/conversations/cliproxy-commandcode-go-plugin.md
- [x] 管理中心配额页显示 CommandCode 实时 5 小时及每周额度，真实浏览器验证窗口及重置时间；原页面已备份
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
