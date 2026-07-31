# 国科大校内共享网关维护手册

> 本文只描述 `shezchen/UCAS_CloudApi` 的校内共享扩展。上游 AxonHub 的通用行为仍以本仓库其他文档为准。
>
> 最后核对：2026-07-28。分支、提交、镜像和生产进程都可能继续变化；维护时必须先读取实际 Git 与运行态，不能把示例当成当前状态。

## 1. 项目目标与不可漂移的原则

这是由学生共同维护、面向国科大校内用户的公益共享网关。设计目标不是把更多操作集中给 Owner，而是让使用者能自助使用、贡献者能自助维护、Owner 只处理全局策略。

以下原则是验收约束：

- 普通用户只能查看和管理自己的 API Key，以及自己捐赠渠道的密钥、配置、有效日期和模型能力覆盖。
- 普通用户不能读取或修改他人的渠道凭证，但所有项目成员都能看到渠道名称、提供商、描述、贡献者、有效期、模型数和健康状态。这是透明度，也是对贡献者的尊重。
- Owner 可以查看和管理全局资源，但不应成为模型目录、渠道健康或日常捐赠操作的人工中转。捐赠渠道的有效期只能由贡献者本人修改且不能清空；有效期内也只能由贡献者本人删除，Owner 不能代改或代删。
- 产品文字统一使用“捐赠渠道”，不要写成容易让新用户误解的“增加渠道”。
- 默认路径先引导“使用”：查看合法模型名、创建自己的 API Key、复制兼容地址；“捐赠”是并列且醒目的贡献路径。
- 捐赠渠道在有效日期前不会因短暂故障被自动删除或永久停用。贡献者主动删除、主动停用或到期才结束其生命周期。
- 公平表示在健康候选中尽量轮换，而不是机械追求每个渠道 Token 完全相等。
- 生产不保存提示词、响应正文或流式分块；短期诊断只保留经过脱敏的错误摘要。
- 构建只在 Mac/可信构建机完成，服务器只接收已经校验的 `linux/amd64` 制品。
- 发布只替换 AxonHub。禁止重启主机、Docker daemon、HAProxy、sing-box、FRP、HCZ、Matrix 或其他无关服务。

## 2. 系统概览

```text
国科大邮箱注册
  → 项目成员登录
  → 创建个人 API Key
  → OpenAI / Anthropic / Gemini 兼容入口
  → 每用户 4 并发 + API Key 自定义配额 + 账户日/周额度
  → 模型访问校验
  → 会话亲和 + 健康候选 + 尽量公平轮换
  → 项目自有渠道或同学捐赠渠道
  → 标准化响应、有效 Token 统计、排行和钱包结算
```

管理面使用 JWT 和项目上下文；模型调用面使用用户创建的 API Key。主要 HTTP 路由位于 `internal/server/routes.go`，GraphQL schema 位于 `internal/server/gql/`。

## 3. 身份、权限与公开范围

| 对象或动作 | 普通成员 | 渠道贡献者 | Owner |
|---|---:|---:|---:|
| 查看合法模型名与模型能力 | 是 | 是 | 是 |
| 查看渠道来源、描述、贡献者与健康 | 是 | 是 | 是 |
| 查看“提供商配额”电池 | 只读 | 只读 | 是 |
| 创建和管理自己的 API Key | 是 | 是 | 是 |
| 查看自己的 API 调用与错误摘要 | 是 | 是 | 是 |
| 读取他人的 API Key 或渠道凭证 | 否 | 否 | 是 |
| 修改捐赠渠道 | 否 | 仅自己的 | 全局管理，但不能代改有效期或代删有效捐赠 |
| 修改渠道模型能力 | 否 | 仅自己的渠道覆盖 | 在高级模型页维护全局模型；不能使用贡献者覆盖接口 |
| 修改全局日/周额度与友链 | 否 | 否 | 是 |
| 刷新或重置提供商配额 | 否 | 否 | 是 |

昵称会优先用于排行和贡献者展示；没有合法昵称时，系统使用项目内稳定、不可反推出数据库用户 ID 的“同学-xxxxxxxx”别名。

API Key 名称只是用户自定标签，不做全局或本人范围的唯一性检查；同一个人也可以创建多个同名 Key，用后四位区分。

关键实现：

- 校内身份与公开别名：`internal/server/biz/user.go`、`internal/server/biz/campus_identity.go`
- API Key 所有权：`internal/server/biz/api_key.go`
- 渠道隐私与贡献者权限：`internal/server/biz/channel.go`、`internal/server/biz/campus_catalog.go`
- GraphQL 授权：`internal/authz/`、`internal/server/gql/*resolvers.go`

## 4. 注册与邮件验证

公开注册只接受以下精确邮箱域名，不接受子域或相似拼写：

- `@mails.ucas.ac.cn`
- `@ucas.ac.cn`
- `@mails.ucas.edu.cn`
- `@ucas.edu.cn`

流程是先调用 `POST /admin/auth/signup/verification` 发送六位验证码，再调用 `POST /admin/auth/signup`。公开注册创建的永远是非 Owner 成员；已有 Owner 身份不由注册接口替换。

默认防滥用参数：

| 参数 | 默认值 |
|---|---:|
| 验证码有效期 | 10 分钟 |
| 同地址重发冷却 | 1 分钟 |
| 单邮箱每小时 | 3 次 |
| 单来源 IP 每小时 | 20 次 |
| 全局每小时 | 200 次 |
| 最大验证尝试 | 5 次 |

SMTP 密码必须通过 `AXONHUB_SMTP_PASSWORD` 注入，不得写入仓库、Compose 或文档。配置结构见 `conf/conf.go` 和 `config.example.yml`，发送实现见 `internal/server/mail/`。

排障顺序：

1. 确认邮箱属于允许域名且已规范化为小写。
2. 确认公开接口返回码；`429` 是限流，`503` 表示验证服务不可用。
3. 只核对 SMTP 主机、端口、发件地址和环境变量是否存在，不输出授权码。
4. 邮件“能发出”不等于注册成功；继续核对验证码是否过期、是否已消费、尝试次数和项目初始化状态。

## 5. 普通用户的使用路径

“共享资源”页是普通用户的主入口，应持续保证以下信息同时可见：

1. 如何使用：兼容 Base URL、创建 API Key 的入口、可直接复制的合法模型名称。
2. 自己的额度：今日/本周有效 Token、重置时间、永久钱包余额。
3. 可用模型：模型名、视觉、工具、推理、上下文和最大输出能力。
4. 共享渠道：名称、提供商、来源、贡献者、描述、有效期、完整可展开模型列表、可测的提供商配额或明确的不可测状态、项目累计有效 Token 和健康详情。
5. 如何贡献：醒目的“捐赠渠道”入口和对回馈规则的简明说明。
6. 透明统计：用户用量排行、模型用量排行和贡献者信息。

创建 API Key 后，用户用自己的 Key 调用兼容 API；用户不是在客户端里“新增一个上游渠道”。前端文案必须明确区分这两个概念。

## 6. 渠道捐赠、模型同步与能力维护

### 6.1 渠道生命周期

贡献者创建渠道时设置有效日期，并可使用 AxonHub 原生提供商认证设计，包括不以静态 API Key 为核心的 Coding Plan。自定义 URL 保留，不做不必要的一刀切拦截；服务端仍必须执行 URL 安全与凭证保护。

普通成员看得到渠道的公开归属和健康信息，但只有贡献者本人和 Owner 能读写通用配置。有效期只能由贡献者修改且不能清空，有效期内也只有贡献者本人能删除。贡献者渠道退出或到期后不再进入生产候选；历史统计和已经获得的钱包回馈不会消失。

### 6.2 自动模型目录

渠道新增或更新后，系统同步其模型列表，并维护同名模型关联及新模型目录。Owner 不需要为每个渠道手工建模。

渠道模型列表有两种明确的维护模式：

1. **自动同步模式**：提供商发现到的模型与贡献者手工补充的模型动态合并。后续同步可以加入提供商新模型。
2. **自定义权威模式**：当前保存的列表就是该渠道应公开和参与路由的完整列表。贡献者删除提供商模型、清空列表，或只保留发现列表的一个子集时，前端必须关闭自动同步并保存当前列表，不能在再次编辑或保存后自动全选、回填或反弹。

贡献者可以重新启用自动同步；下一次同步会恢复提供商目录与手工模型的动态合并。切换模式和编辑模型列表都是贡献者自己的维护责任，不转嫁给 Owner。

优先顺序：

1. 使用已知模型目录中的正式能力资料。
2. 合并贡献者对自己渠道所做的能力覆盖。
3. 对自定义 URL 或未知新模型使用宽容的默认值：
   - 视觉：支持
   - 工具调用：支持
   - 推理：支持
   - 上下文：1,000,000 Token
   - 输入模态：文本和图像
   - 输出模态：文本

这些默认值是网关的能力声明，不保证每个上游真的达到该上限。贡献者应在“共享资源”页修正自己渠道的视觉、工具、推理、上下文和最大输出能力。

`/v1/models` 对推理模型统一声明 `low`、`medium`、`high`、`xhigh`、`max` 五档。它帮助客户端展示和透传，不代表网关替上游裁决每个档位的真实支持情况。

关键实现：

- 列表同步：`internal/server/biz/channel_model_sync.go`
- 自动建模与默认能力：`internal/server/biz/model_catalog.go`
- 贡献者能力覆盖：`internal/server/biz/campus_catalog.go`
- OpenAI 模型能力输出：`internal/server/api/openai.go`

## 7. 配额、有效 Token 与捐赠钱包

### 7.1 三层限制

调用按以下顺序受约束：

1. API Key/Profile 自定义配额：用户可单独给自己的每个 Key 设置限制。
2. 账户日/周全局额度：Owner 设置一组全局默认值，实时作用于所有已有和未来账户，不存在按用户例外。
3. 每用户并发：同一用户所有 API Key 合计最多 4 个在途请求，超出返回 HTTP `429` 和错误码 `concurrency_limit_exceeded`。

默认账户额度：

| 窗口 | 默认上限 | 重置 |
|---|---:|---|
| 每日 | 16,000,000 有效 Token | 北京时间每日 00:00 |
| 每周 | 64,000,000 有效 Token | 北京时间每周一 00:00 |

Owner 在用户管理页修改的是这两个对所有账户统一生效的默认值。系统在每次配额检查时读取设置，修改后下一次检查即生效；设置为 `0` 会阻止普通额度承接新的计量请求。

并发限制当前是单进程内存计数，适用于单实例 V1。扩展为多实例前必须换成共享原子计数，否则每实例都会各自允许 4 并发。

### 7.2 有效 Token 口径

日额度、周额度、排行和捐赠钱包统一使用“扣除缓存读取后的有效 Token”：

```text
非缓存输入 = max(prompt_tokens - cached_read_tokens, 0)
有效 Token = max(非缓存输入 + completion_tokens,
                 total_tokens - cached_read_tokens)
```

只扣除缓存读取；缓存写入仍是实际输入。`completion_tokens` 已包含推理 Token，不重复相加。若兼容上游没有报告缓存读取明细，系统无法猜测未知缓存，只能按保守口径计量。为防恶意伪造，单请求结算最多计 10,000,000 有效 Token。

实现位于 `internal/server/biz/effective_tokens.go`。

### 7.3 永久钱包

钱包从新版本首次记录的切换时间起累计，不追溯历史。一次成功、非测试调用满足条件时，贡献者得到：

```text
回馈 = floor(该请求有效 Token / 2)
```

以下情况不产生回馈：

- 手动渠道测试；
- 贡献者使用自己的渠道；
- Owner 自有渠道；
- 钱包切换时间之前的调用；
- 没有有效 Token 的调用。

用户调用时先消耗永久钱包余额；余额不足的部分再进入普通日/周额度。API Key 自定义配额仍先于钱包检查，因此钱包不会绕过用户自己给 Key 设置的限制。

钱包采用物化余额加只增账本，成功用量事务是唯一结算点。备份与恢复必须同时包含：

- `user_token_wallets`
- `token_wallet_ledgers`
- 与账本关联的 usage/request/channel/user 身份

关键实现和恢复校验位于 `internal/server/biz/token_wallet.go`、`internal/server/backup/wallet_ops.go`、`internal/server/backup/wallet_restore.go`。

### 7.4 配额边界

配额按请求开始前的已结算用量检查，没有预占。单个大请求或多个并发请求可能使已结算值短暂越过边界；下一次新请求会被阻止。维护时不能把它描述成严格的请求内硬截断。

## 8. 公平轮换、会话亲和与故障转移

### 8.1 选择原则

生产候选先满足“已启用、未过期、支持目标模型、当前健康”。在这些候选里，轮换器偏向近期被选中会话较少的渠道，并在空闲后衰减。已命中会话亲和的后续请求和手动测试不推进公平计数。目标是长期尽量公平，而非为了追平 Token 数机械切换。

同一对话优先留在已经成功的渠道，以保护上游缓存和降低风控：

- 客户端有显式 trace/session 线索时，显式亲和优先。
- 没有 session 标识时，系统取 system/developer/首条 user 上下文前缀，生成项目与用户隔离的 HMAC 摘要。
- 原始提示词不进入亲和表；摘要输入最长 8 KiB，进程内最多 4,096 项，滑动有效期 6 小时。
- 只有收到语义有效输出后才绑定渠道；普通文本、纯工具调用、推理、拒答和多媒体都属于有效输出。

会话亲和和公平计数也是单进程状态。多实例部署前需要共享存储，或明确接受跨实例失效。

### 8.2 失败处理

- 空响应、可重试 HTTP 状态和没有状态码的 TLS/连接/解码/超时故障可重试。
- 同一渠道重试一次，仍失败再切换健康渠道。
- 流在首个语义内容提交前结束且没有终态事件时，按不完整流失败处理并进入同请求故障转移。
- 明确的 `model not supported` 会先跳过当前渠道中指向同一实际模型的重复映射，再尝试其他模型或渠道；`429` 配额/限流直接进入跨渠道选择，不在同一渠道反复消耗重试预算。
- 默认未配置的 `401/403` 不自动重试或故障转移，避免把认证错误放大到其他渠道。
- 已经提交给客户端的流式输出不能透明重放；应记录渠道不健康，但不能伪造无感重试。
- 解码成功却没有任何语义输出仍视为失败；纯工具调用不能误判为空响应。
- 捐赠渠道发生波动时进入冷却或断路器并暂时退出候选；恢复后重新参与轮换，不自动永久禁用。

首次迁移会设置轮询、单渠道最多重试一次和空响应检测。迁移标记为 `campus_sharing_policy_v1`，只执行一次，之后不会覆盖 Owner 的主动修改。

正式服务采用单渠道重试 `1` 次、跨渠道最多 `10` 次、首事件超时 `90` 秒。超时只约束首个上游事件，用于结束无响应的挂起连接；它不会截断已经开始正常输出的长响应。

### 8.3 Codex Responses Lite 兼容

Codex 的 Responses Lite 请求头与 `reasoning.context: "all_turns"` 是一个不可拆分的不变量。网关在 Responses 类型转换中保留该字段，并在渠道 body/header 覆盖全部执行后再次校正，因此 HTTP/SSE 与 WebSocket 都不会把“有 Lite 请求头、无对应 context”的非法请求发给上游。若仍出现 `unsupported_value`，应先核对最终上游请求元数据和客户端版本，不要归因于代理出口。

关键实现：

- 轮换评分：`internal/server/orchestrator/lb_strategy_rr.go`
- 会话亲和：`internal/server/orchestrator/session_affinity.go`
- 重试分类：`internal/server/orchestrator/retry.go`
- Responses Lite 不变量：`llm/transformer/openai/codex/outbound.go`、`internal/server/orchestrator/override.go`
- 语义成功与健康：`internal/server/orchestrator/performance.go`
- 捐赠渠道禁用保护：`internal/server/orchestrator/channel_auto_disable.go`
- 一次性策略迁移：`internal/server/biz/system.go`

## 9. 健康状态与手动测试

共享渠道 UI 支持五态：

- `healthy`：最近成功且没有显著失败；
- `degraded`：成功与失败并存；
- `unhealthy`：近期只有失败或被管理性停用；
- `recovering`：冷却结束后的恢复观察阶段；
- `unknown`：没有足够生产或测试证据。

所有项目成员都能测试公开渠道。测试由服务端在授权后使用该渠道真实配置执行，不要求普通用户拥有或读取渠道密钥。

模型候选不是固定拿列表第一个，而是：

1. 仍存在于同步列表的渠道默认测试模型；
2. 最近真实 API 成功使用过的模型；
3. 其他同步模型，按版本号优先于偶然的数组位置。

只有明确的“模型不存在/不支持/参数无效”类 `400`、`404`、`422` 才尝试下一个模型。认证、限流、TLS、网络和上游故障必须诚实呈现，不用模型回退遮盖。

测试响应展示实际模型、总延迟、HTTP 状态码、尝试链和脱敏后的上游错误原文。每个用户对每个渠道有 30 秒点击冷却。测试：

- 不计入用户日/周额度；
- 不计入排行或公平轮换；
- 不产生或消耗捐赠钱包；
- 会更新生产健康状态。

实现位于 `internal/server/api/campus_catalog.go`、`internal/server/biz/campus_catalog.go`。

## 10. 透明统计、诊断与隐私

### 10.1 排行

用户用量和模型用量都默认展示当日，并可切换本周、本月。模型榜最多 50 条；用户榜保留前 50，并在本人不在前 50 时额外附上本人一行。用户榜优先昵称。模型榜拆分有效 Token、输入、缓存读取、输出和计量请求数。

排行只统计真实 API 计量调用，不统计手动测试。月榜没有对应的月额度，因此不显示伪造的月限额比例。实现位于 `internal/server/gql/campus_leaderboard.go`。

### 10.2 用户自己的 API 活动

每个用户只能查看自己 API Key 最近 6 小时的调用诊断，可按 Key 筛选。最多读取 200 个请求并返回 100 条事件。面板上方按 Key 汇总输入、缓存读取、输出、有效 Token 及成功/错误次数；事件列表包含时间、Key 名称与后四位、模型、状态、HTTP 状态、总延迟和脱敏错误。当前事件 DTO 不提供单次 Token 明细或首 Token 延迟。

明确不提供：

- 提示词和响应正文；
- 请求/响应 Header；
- IP 与完整 URL；
- 完整 API Key；
- 其他用户的活动。

错误摘要最多 600 个字符，并清除凭证、Cookie、查询参数中的敏感值和邮箱；调度器每 15 分钟清理超过 6 小时的错误文字。请求的非正文统计仍可继续用于额度和排行。

实现位于 `internal/server/biz/campus_catalog.go`、`internal/server/biz/request.go` 和 `internal/server/scheduler/`。

### 10.3 正文存储是生产不变量

上游代码默认存储策略仍可能开启请求/响应正文。校内生产依赖数据库中的 `storage_policy` 覆盖，发布、恢复或新建数据库后必须核对：

```json
{
  "store_chunks": false,
  "store_request_body": false,
  "store_response_body": false
}
```

不能只看 UI，也不能因为短期活动面板已经脱敏就推断正文存储关闭。禁止在终端、工单或 GitHub 输出其他 `systems` 配置行、正文、API Key、SMTP 授权码或渠道凭证。

## 11. 渠道透明度、模型跑分与友链

### 11.1 公开渠道卡

“提供商配额”电池对所有已登录项目成员可见，数据来自启用且未过期渠道的严格字段白名单。普通成员只读，不能刷新、重置或管理；Owner 保留管理动作。它表示上游供应商/Coding Plan 配额，不是账户的 16M/64M 校内有效 Token 额度。

“共享资源”里的每张公开渠道卡还必须：

- 在提供商返回可量化窗口时，显示最紧张窗口的剩余百分比和已知重置时间；
- 在提供商不支持查询、未返回可量化字段或加载失败时，诚实显示“不可测量”、加载中或读取失败，不能把缺失数据解释成 `0%` 已用或 `100%` 剩余；
- 首屏预览少量模型，并提供明确按钮展开完整模型列表；模型名必须完整换行显示，不能用省略号代替用户需要复制的合法名称；
- 显示该渠道在当前项目内累计产生的有效 Token。口径与 7.2 一致，排除手动测试；该数字不是上游账户的终身总消耗。

实现位于 `internal/server/biz/campus_catalog.go`、`internal/server/api/provider_quota_view.go`、`frontend/src/features/campus-resources/`、`frontend/src/features/system/data/quotas.ts` 和 `frontend/src/components/quota-badges.tsx`。

### 11.2 模型跑分与成本

“模型跑分与成本”页直接嵌入 Artificial Analysis 官方 `https://artificialanalysis.ai/embed/llm-leaderboard` iframe，并保留打开其官方模型榜单的链接。页面用同一原始榜单帮助用户比较模型能力与成本；加载失败时只展示失败说明和官方直达链接。

维护边界：

- 不抓取、缓存、代理、裁剪、重绘或二次发布榜单数据；
- 不读取或改写 iframe 内部 DOM；
- 外部榜单只提供选型参考，不参与渠道路由、健康判断、校内额度、钱包回馈或账单计算；
- 实际可调用模型仍以“共享资源”和 `/v1/models` 为准，Artificial Analysis 没有收录不等于渠道不可用。

实现位于 `frontend/src/features/model-benchmarks/` 和 `frontend/src/routes/_authenticated/project/model-benchmarks/`。

### 11.3 友链

友链由 Owner 在系统常规设置中维护，名称和 URL 必填，描述选填；只允许无凭证的绝对 `http://` 或 `https://` 地址，名称和 URL 不重复，最多 20 条。普通成员在侧栏只读查看。

实现位于 `internal/server/biz/system.go`、`internal/server/gql/system.resolvers.go` 和 `frontend/src/features/system/components/general-settings.tsx`。

## 12. 校内扩展接口

| 方法 | 路径 | 权限与用途 |
|---|---|---|
| `POST` | `/admin/auth/signup/verification` | 公开，申请国科大邮箱验证码 |
| `POST` | `/admin/auth/signup` | 公开，验证后创建普通成员 |
| `GET` | `/admin/campus/resources` | 项目成员，模型、渠道模型/健康/累计有效 Token、账户额度和钱包汇总 |
| `GET` | `/admin/campus/api-activity` | 项目成员，仅自己的 6 小时活动 |
| `POST` | `/admin/campus/channels/:id/probe` | 项目成员，测试公开渠道 |
| `GET` | `/admin/campus/channel-model-capabilities` | 贡献者，自己的渠道能力 |
| `PATCH` | `/admin/campus/channel-model-capabilities` | 贡献者，修改自己的能力覆盖 |
| `GET` | `/admin/provider-quotas` | 项目成员，提供商配额只读投影 |
| `POST` | `/admin/graphql` | JWT；排行、Owner 设置及通用管理 |
| `GET` | `/v1/models` | API Key；返回可调用模型和能力声明 |

所有 `/admin/campus/*` 和 `/admin/provider-quotas` 成功响应都应使用 `Cache-Control: private, no-store`。

## 13. 代码地图

| 领域 | 主要入口 |
|---|---|
| 路由与中间件 | `internal/server/routes.go` |
| 邮箱注册 | `internal/server/api/auth.go`、`internal/server/biz/auth.go` |
| 用户与 UCAS 域名 | `internal/server/biz/user.go` |
| 共享资源、渠道模型/累计用量、健康、活动、能力覆盖 | `internal/server/biz/campus_catalog.go` |
| 渠道测试 HTTP | `internal/server/api/campus_catalog.go` |
| 自动模型目录 | `internal/server/biz/model_catalog.go`、`channel_model_sync.go` |
| 有效 Token | `internal/server/biz/effective_tokens.go` |
| 日/周额度 | `internal/server/biz/quota.go`、`user_daily_quota_settings.go` |
| 钱包与结算 | `internal/server/biz/token_wallet.go` |
| 公平轮换与亲和 | `internal/server/orchestrator/lb_strategy_rr.go`、`session_affinity.go` |
| 错误重试与健康 | `internal/server/orchestrator/retry.go`、`performance.go` |
| 用户/模型排行 | `internal/server/gql/campus_leaderboard.go` |
| 提供商配额 | `internal/server/api/provider_quota_view.go` |
| 共享资源与渠道配额前端 | `frontend/src/features/campus-resources/`、`frontend/src/features/system/data/quotas.ts` |
| 渠道模型选择前端 | `frontend/src/features/channels/components/channels-action-dialog.tsx`、`frontend/src/features/channels/utils/model-selection.ts` |
| 模型跑分与成本前端 | `frontend/src/features/model-benchmarks/`、`frontend/src/routes/_authenticated/project/model-benchmarks/` |
| API 活动前端 | `frontend/src/features/apikeys/` |
| Owner 全局额度 | `frontend/src/features/users/components/user-daily-quota-settings-card.tsx` |
| Owner 友链 | `frontend/src/features/system/components/general-settings.tsx` |
| 钱包备份恢复 | `internal/server/backup/wallet_ops.go`、`wallet_restore.go` |

## 14. 修改与验证矩阵

不要把“编译成功”当成业务验收。涉及以下领域时至少覆盖对应验证：

| 修改领域 | 最低验证 |
|---|---|
| 注册 | 四个合法域、相似域拒绝、验证码过期/重放、SMTP 失败关闭、Owner 不被创建 |
| API Key | 同名 Key 成功、所有权隔离、后四位可区分 |
| 渠道 | 贡献者 CRUD、他人只读公开投影、有效期内 Owner 不代删、到期、凭证不泄露 |
| 模型同步 | 同名关联、新模型、未知模型默认、贡献者覆盖、`/v1/models` 推理档位；自动模式合并提供商/手工模型；删除、清空和子集选择切换为权威列表且编辑保存不反弹；重新启用后恢复动态同步 |
| 配额 | 16M/64M 默认、Owner 修改实时全局生效、北京日/周边界、Key 配额先行、并发越界边界 |
| 钱包 | 50% 向下取整、自用/Owner/测试排除、钱包先扣、并发结算、备份恢复 |
| 轮换 | 健康候选尽量公平、会话粘连、恢复重入、空响应、纯工具调用、无状态错误 |
| 渠道测试 | 自适应模型、真实状态码/错误、仅模型不支持才回退、30 秒冷却、不参与计量 |
| 隐私 | 正文/分块关闭、错误脱敏、6 小时清理、跨用户活动不可见 |
| 共享资源 UI | 使用与捐赠路径、所有信息未丢失、健康五态、贡献者、两类排行、配额电池；渠道配额可测/不可测分明、模型完整展开、累计有效 Token 正确、桌面与移动端无溢出 |
| 外部模型榜单 UI | Artificial Analysis 原始 iframe 可加载，加载中/失败回退和官方直达链接可用；页面不影响路由、健康、额度、钱包或计费 |

仓库已有对应单元测试；新增语义必须补测试，而不是只改前端展示。

## 15. 安全发布

### 15.1 发布前

1. 在 Mac 检查 `git status --short --branch`，确认目标提交和未跟踪文件。
2. 从明确提交用 `git archive` 创建干净构建目录，不能直接复制脏工作区。
3. 使用 Node 20、pnpm 10 构建前端，再把完整 `frontend/dist` 嵌入 Go。
4. 构建 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 制品，并注入版本、完整提交和构建时间。
5. 校验镜像平台、入口、内容与 SHA-256 后再上传。
6. 服务器上禁止构建，禁止 `docker prune`。

仓库根 `docker-compose.yml` 是上游 PostgreSQL 示例，不是腾讯云正式 Compose，生产禁止直接使用。

### 15.2 生产预检

先只读确认真实 SSH 目标、hostname、磁盘、Compose 文件、服务名、容器、镜像、挂载和网络。不得输出完整环境变量、完整 `docker inspect`、原始凭证或原始请求日志。

保存基线：

- AxonHub 的容器 ID、镜像、启动时间、重启次数和数据卷；
- 所有其他容器的 ID 与启动时间；
- HAProxy、sing-box、FRP 等受保护服务的 PID 和启动时间；
- 两份生产 Compose 的备份和哈希。

### 15.3 数据快照

AxonHub 启动会自动执行数据库迁移，因此切换前必须：

1. 只停止 AxonHub，禁止 `compose down`。
2. 原样备份 `axonhub.db`、现存 WAL 和 SHM。
3. 使用 SQLite backup API 生成合并 WAL 的一致性快照。
4. 对快照运行 `PRAGMA quick_check` 和 `PRAGMA foreign_key_check`。

实时 `axonhub.db`、WAL、SHM 不是“旧备份”，禁止作为清理对象删除。

### 15.4 切换

只更改正式 Compose 覆盖文件中的 AxonHub 镜像，然后：

```bash
docker compose -f <base-compose> -f <production-override> config --images
docker compose -f <base-compose> -f <production-override> up -d --no-deps --force-recreate axonhub
```

禁止重启 Docker daemon、主机和任何代理/转发/其他应用服务。

### 15.5 上线门槛

必须全部通过：

- 内网与公网 `/health` 返回 200，构建提交是目标提交；
- 根 HTML 返回 200，HTML 引用的每个本项目 JS/CSS/favicon 资源都返回 200；
- 未认证 `/v1/models` 返回 401；
- 未认证 `/admin/provider-quotas` 返回 401；
- 已认证项目成员能读取资源和只读提供商配额，非成员被拒绝；
- 普通成员能测试渠道并看到实际模型、状态码和脱敏错误；
- SQLite `quick_check=ok` 且外键错误为 0；
- `storage_policy` 三个正文/分块开关仍为 `false`；
- 数据卷没有变化，AxonHub 重启次数正常；
- 其他容器和 HAProxy/sing-box/FRP 的基线完全不变。

`/health` 不检查数据库，也不能证明前端静态资源完整，所以不能单独作为上线依据。

### 15.6 生产出口粘滞策略

服务器本地 sing-box URLTest 组使用 `tolerance: 30000` 毫秒、`interval: 61s` 和 `interrupt_exist_connections: false`。当前出口的健康探测成功时保持不变，仅在该探测失败后故障转移；恢复或延迟更低的节点不得抢占仍然健康的当前出口。

生产代理配置和全部凭据只保留在服务器本地，绝不提交。代理配置热重载后，必须确认 sing-box 的 PID、重启次数和监听端口均未变化，并确认受保护的 FRP/HY2 服务及 AxonHub 均未受影响。

## 16. 回滚

1. 只停止失败的新 AxonHub。
2. 将新版本产生的数据库文件整体移入隔离目录，保留供核对，禁止删除。
3. 恢复旧 Compose 镜像引用。
4. 数据库仍兼容时，优先用旧镜像读取当前数据库，避免丢掉切换后的新写入。
5. 只有迁移或数据结构不兼容时才恢复发布前一致性快照。
6. 仍只用 `up -d --no-deps --force-recreate axonhub`。
7. 重新执行 HTTP、静态资源、数据库、隐私和受保护服务基线验证。

恢复旧快照会回到切换前状态，不能声称保留新版本开放期间的所有写入。失败版本数据库必须保留以便后续核对和必要的数据迁移。

## 17. 常见症状与判断

| 症状 | 先检查 | 不要先做 |
|---|---|---|
| 网页白屏 | 根 HTML、每个引用资源、浏览器控制台、提交是否嵌入前端 | 重启代理或主机 |
| `/v1/models` 返回 401 | 是否未带网关 API Key；未认证 401 是正确行为 | 改为公开接口 |
| Owner 测试绿、普通成员测试失败 | 是否走成员测试接口、模型候选、真实状态码和脱敏错误 | 用泛化“不可用”遮盖错误 |
| 渠道间歇失败 | 健康五态、断路器、同渠道一次重试、出站代理/TLS | 删除捐赠渠道 |
| 地区错误 | 容器内代理变量、Docker 可达代理地址、实际出口 | 修改入站 HAProxy |
| 用户额度异常 | 有效 Token、钱包扣减、日/周北京窗口、Key 自定义配额 | 修改用户遗留字段做例外 |
| 钱包没增加 | 是否测试/自用/Owner 渠道、是否切换时间之后、是否成功结算有效 Token | 手工直接改余额 |
| 活动面板没有旧错误 | 6 小时保留与 15 分钟清理是否正常 | 开启正文存储 |
| 提供商配额为空 | 渠道是否启用/未过期、提供商是否返回白名单字段 | 向普通用户暴露原始 quota JSON |

## 18. 维护提交清单

每次功能修改都应：

1. 先记录分支、HEAD 和脏文件，保护用户已有改动。
2. 删除已经被替代的僵尸代码，不保留两套逻辑。
3. 同时更新中文和英文文档、UI 中英文文案及对应测试。
4. 不提交 `.env`、授权码、渠道密钥、数据库、日志、构建缓存或镜像归档。
5. 一个可独立回退的步骤一个提交，使用 `feat`、`fix`、`refactor`、`chore` 或 `backup`。
6. 发布前打 `save-YYYY-MM-DD-描述` 标签。
7. 推送后核对远端分支和标签指向同一预期提交。

当业务语义发生变化时，本手册是必须同步更新的维护入口，而不是上线后的补记。
