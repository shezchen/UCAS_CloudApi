# 校内共享网关统一公平路由与故障转移

> 状态：实现完成；自动验证已通过，生产发布证据见 §12
> 日期：2026-08-03
> 范围：聊天与 Responses 请求的渠道选择、会话亲和、TestChannel 可用性、透明重试、诊断 UI
> 结论先行：生产路由只保留一套逻辑——**稳定的外层渠道环 + 会话亲和 + 精确 route 的 TestChannel 二值状态 + 有界故障转移**。不使用评分、权重、冷却、生产错误黑名单、自动禁用或第二套负载均衡策略。

---

## 1. 为什么要重写

校内共享与普通商业网关的目标不同：多个同学捐赠的可用渠道应当持续获得公平的使用机会，同时任何一次用户请求又应尽可能由可工作的 route 救回。旧实现同时叠加轮询、指标评分、配额优先、熔断、冷却、自动禁用和状态码重试，规则会互相覆盖，出现以下不可接受的行为：

- 某渠道瞬时抖动一次，此后长期拿不到请求，也就永远没有“恢复”的机会；
- 渠道有多把 key 就获得更多外层流量，破坏捐赠者之间的公平；
- 客户端请求本身有问题，却被生产错误直接判成渠道坏；
- TestChannel 测的是聊天协议或第一把 key，生产失败的却是 Responses 协议或另一把 key；
- 流已经给客户端一部分内容后静默结束，系统却误记成功，或试图重放导致重复内容；
- 多套策略在 UI、API key profile、模板和后端各有入口，管理员无法判断最终是谁在裁决。

本设计把“公平”和“可用性”拆成两个互不篡改的事实：

1. **外层渠道环决定谁获得本次首发机会**；
2. **TestChannel 二值结论只帮助失败后的救援选择**。

---

## 2. 术语与唯一状态模型

### 2.1 Channel、route 与候选集合

- **逻辑模型**：用户请求的合法模型名，例如 `gpt-5.6-sol`。
- **Channel**：一位贡献者捐赠或 Owner 自有的一条渠道。公平份额按 Channel 计算，不按 key 数量计算。
- **精确 route**：一次能够独立执行、独立测试的上游路径：

  ```text
  route = channel × credential × ActualModel × APIFormat × 有效 endpoint/proxy 配置
  ```

  credential 在状态、日志和 UI 中只保留不可逆指纹或掩码，不保存/展示明文。

- **候选集合**：经过模型映射、捐赠有效期、显式管理员禁用、项目边界、协议能力等静态条件过滤后，当前请求真正可执行的 Channel/route。TestChannel FAIL **不是**静态过滤条件。

当前 `RouteKey` 字段为 `ChannelID + CredentialID + ActualModel + APIFormat + ConfigRevision`。`ConfigRevision` 是有效 endpoint 与代理配置的不可逆摘要；修改 URL、协议路径或代理后，旧 verdict 不会被新 route 复用。`CredentialID` 统一覆盖 API key、OAuth/coding-plan、GCP 与其他结构化凭据：OAuth 的短期 access token 刷新保持同一身份，而 refresh token、账号、scope 或实质凭据变化会生成新身份。原始凭据和配置均不进入路由状态、日志或公共 API。

### 2.2 可用性不是分数

对每个精确 route 只保存：

```text
Known = false               尚无完成的权威测试
Known = true, Available = 1 最近完成的权威 TestChannel 为 PASS
Known = true, Available = 0 最近完成的权威 TestChannel 为 FAIL
```

`unknown` 只是“没有 verdict”，不是第三档健康评分。系统中不得出现成功率分、延迟分、失败次数分、惩罚分、冷却截止时间或由生产请求直接写入的健康值。

### 2.3 唯一写入者

只有**完成了正常判定流程的 TestChannel**可以写 `Available=1/0`：

- 手工“测试渠道”和生产异常触发的程序化测试必须走同一个判定器；
- 程序化测试必须强制使用刚才生产尝试的精确 credential、ActualModel、APIFormat 和有效 endpoint/proxy；
- 生产成功、HTTP 400/401/402/429/5xx、超时、转换失败、空流或不完整流都不能直接写可用性；
- 测试本身 panic、取消、超时、内部依赖失败或未形成完整 verdict 时，不覆盖旧状态；
- 同一 route 并发触发测试时使用 singleflight；不同批次用“测试开始代数”防止较早开始、较晚返回的旧 FAIL 覆盖新 PASS。

正常文本和**纯工具调用**都属于 PASS；不能用“必须有 output token”作为唯一判断。只有 usage、心跳、伪 `[DONE]` 或没有协议完整性的响应不属于 PASS。

---

## 3. 首发：稳定外层 Channel 环

### 3.1 数学定义

对每个逻辑模型 `m`，取有序且去重的 eligible Channel ID 集合：

```text
C(m) = sorted(unique(channel IDs))
```

状态只记上次选中的 Channel ID `last(m)`。下一次需要给**新会话**分配首发渠道时：

```text
next(m) = C(m) 中第一个大于 last(m) 的 ID；若不存在则取 C(m) 的第一个
last(m) = next(m)
```

这是连续游标，不按小时、天、请求批次或任何“周期”重置。成员增加/删除时也不做 `index % len`，避免集合变化造成游标漂移。若 A、B、C 都 eligible，新会话首发顺序就是 `A, B, C, A, B, C...`。

### 3.2 公平边界

- `Available=0` 的 Channel 仍保留未来每一个应得的首发位置；这是其自动恢复机会。
- 一个 Channel 有 10 把 key，另一个只有 1 把 key，外层份额仍为 1:1；key 只在本 Channel 内部轮换。
- 同一 Channel 有多个 ActualModel 映射或多个 endpoint，也只占一个外层位置。
- 每个逻辑模型有独立游标；模型 X 的请求不推进模型 Y 的公平位置。
- 救援尝试使用独立游标，不能偷走或推进未来的首发位置。
- 捐赠到期、捐赠者删除、Owner 显式禁用、模型静态不支持属于 candidate eligibility 变化；它们与 TestChannel FAIL 不同，可以不进入候选集合。

---

## 4. 会话亲和与无 session 客户端

1. 有显式 session/trace 且已有成功绑定时，继续使用绑定 Channel，不推进新会话公平游标。
2. 没有 session 标识时，用“系统/开发者消息 + 第一条用户消息”的稳定上下文前缀生成 HMAC 摘要；摘要按 project/user 隔离，不保存提示词正文。
3. 首次未绑定的新会话调用外层环一次；只有正常文本或纯工具调用等语义成功后才绑定。
4. 绑定 Channel 失败并被其他 Channel 救回后，把会话重绑到真正成功的 Channel。
5. 绑定目标已删除、过期、被静态过滤或不支持该模型时，必须回到 `next(m)`，而不是固定落到排序第一项，也不能冻结公平游标。
6. 并发的同一未绑定会话需要验证不会被当成多个新会话推进多次；若当前实现不能保证，应明确接受边界或加单航班/原子绑定测试。

亲和只决定首发 Channel，不给予一个已失败 route 永久豁免；失败后仍进入统一救援。

---

## 5. 生产异常、程序化 TestChannel 与救援

### 5.1 什么是生产异常

不维护错误码白名单或可重试错误枚举。只要本次尝试没有形成对客户端协议而言正常、完整的输出，就进入失败路径，例如：

- 任意 HTTP/上游错误（400、401、402、429、5xx 以及无状态码错误）；
- 连接失败、TLS、首事件超时、整体超时、转换/解析错误；
- 非流式响应没有语义内容；
- 流在客户端提交前结束、只含 usage/心跳、终止为 failed/incomplete；
- Responses 只有 `[DONE]`，没有有效 `response.completed`；
- 流提交后有内容但最终缺少正确 terminal event。

生产异常并不等价于“渠道坏”。例如用户发了非法 `item_` ID 导致 400，程序化 TestChannel 仍可能 PASS，此时 route 继续为 1。这样同时满足“用户请求必须诚实报错”和“客户端错误不能误杀渠道”。

### 5.2 异步精确测试

每次生产异常都立即：

1. 冻结本次精确 route 快照，不能稍后从会被下一次重试修改的 context 中重新读取；
2. 异步触发相同 route 的 TestChannel；
3. 不等待测试返回，直接按当前已知状态选择救援；
4. 测试 context 必须与生产重试的可变 context 容器隔离，避免 credential/source 被并发覆盖。

生产在 `unknown` 或已知 FAIL route 上成功时，也异步触发 TestChannel 以确认恢复；生产成功本身仍不直接改状态。

### 5.3 唯一救援顺序

请求内维护 `AttemptedRoutes`，去重粒度必须是精确 route，不能只按 Channel 去重。每次救援选择时重新读取 TestChannel 状态：

```text
1. 未尝试的 PASS 精确 routes
2. 已尝试的 PASS 精确 routes（需要重复时按独立的简单环）
3. 如果完全没有 PASS：未尝试的所有可执行精确 routes
4. 如果完全没有 PASS 且 distinct routes 已用完：所有 routes 中重复 best-effort
```

约束：

- 只要在某次选择时存在 PASS，救援就不得先选 FAIL/unknown；
- 若请求到达 Owner 配置预算内的最后一次救援，且此刻存在 PASS，最后一次必须选 PASS；“known-good”只表示选中时最近 TestChannel 为 PASS，不保证上游下一毫秒一定成功；
- 完全没有 PASS 时不能宣告全站不可用，也不能把全部 Channel 冷却；应先让每个 distinct route 获得机会，再在剩余预算内重复 best-effort；
- 某 route 为 FAIL 时，它仍会在将来的公平首发位置再次被实际尝试。成功则直接服务用户，并异步 TestChannel；失败则依靠本请求的救援兜底；
- 同一 route 重复、同 Channel 重试、换 ActualModel、换 key、换协议不能各自拥有另一套错误分类策略。

### 5.4 Owner 的唯一重试配置

Owner UI 只暴露：

- 是否启用重试；
- 跨 route/Channel 的最大救援次数（总尝试数 = 1 + 救援次数）；
- 流首事件超时；
- 非流响应超时。

不得暴露或暗中启用：负载均衡策略选择、同 Channel 特判重试、retry delay、按状态码重试列表、自动禁用、健康评分/冷却、空响应检测开关。空/不完整响应检测是正确性要求，必须始终开启。

如果 Owner 把最大救援次数设为 0 或关闭重试，系统只能做一次首发；UI 必须明确此时没有故障转移，不能仍承诺“最后一次 known-good”。

---

## 6. 流式协议边界

透明故障转移受“客户端是否已经看到数据”这一不可绕过的边界约束：

- **提交前**：尚未向客户端交付任何语义事件时，空流、错误 terminal、超时或转换失败可以丢弃本次尝试并换 route；
- **提交后**：已交付文本或工具调用后，不得从另一 route 重放整个回答，否则会重复文本/工具副作用。此时必须如实以错误结束、记录失败、异步 TestChannel，不能伪造成功；
- 正常文本和纯工具调用都算语义输出；reasoning/usage/心跳本身不能证明完整成功；
- Chat Completions、Responses、Anthropic 等分别使用各自的真实 terminal 语义。Responses 的传输 `[DONE]` 不是 `response.completed` 的替代品；`response.failed` / `response.incomplete` 必须保持失败/不完整。

因此，“任何错误都要尽可能救回”不等于“提交后也盲目重放”。上线验收必须分别覆盖提交前和提交后。

---

## 7. Responses / Codex 协议修复

本轮与路由一起守住以下协议不变量：

1. 上游返回的 `msg_...` message ID 原样保留；
2. 网关必须生成 message ID 时也生成可重放的 `msg_...`，不能把 `item_...` 作为后续 message 引用发送回上游；
3. 多轮转换保留 message/refusal 的生命周期与引用关系；
4. Codex Responses Lite 强制 `reasoning.context = "all_turns"`，包括 pass-through/override 路径；
5. 只有真实、可解析且状态 completed 的 Responses terminal 能记成功，伪 `[DONE]`、failed/incomplete snapshot 均不能记成功。

已存在独立提交 `32a6d2f4 fix(responses): preserve replayable message identifiers`，但最终发布仍需把它与本轮路由测试一起完整回归。

---

## 8. 可观测性、隐私与计量

- 渠道页展示 TestChannel 的**精确 route**结论：测试模型、ActualModel、协议、匿名 route 标识、测试时间、HTTP 状态和脱敏后的上游错误原文。公共页面不得展示 credential 明文、前后缀或可用于撞库的稳定标识；捐赠者/Owner 的管理页也只能使用既有安全掩码。
- 如果卡片只汇总一个 route，必须明确标成“最近测试的 route”，不能把某一把 key/某一协议的绿色冒充整个 Channel 全绿；更好的目标是显示 PASS/FAIL/unknown route 数并可展开。
- 用户 API 活动显示真实尝试链、每次 route/Channel 结果、最终由谁救回和脱敏错误；不能只写泛泛的“可用/不可用”。
- 脱敏只移除密钥、token、授权头和可能的敏感正文，不得把实际错误改成无排障价值的通用文案。
- TestChannel 请求不计入用户日/周额度、钱包回馈、捐赠用量和排行榜，但会更新 TestChannel route 状态。
- 仍只保存排障所需元数据；不因本设计保存提示词或响应正文。
- 可用性内存状态重启后回到 unknown 是安全降级：公平首发照常，生产成功/失败会重新触发测试。UI 不得把 unknown 显示为 FAIL。若将来需要持久化，必须另行设计过期、配置修订和隐私边界。

---

## 9. 对抗反例与验收矩阵

### 9.1 证据状态说明

本表的“状态”只说明证据是否已写入仓库，**不代表测试已经通过**：

- `AUTO-EXISTS`：已有自动化测试或明确测试占位；最终统一测试结果待发布记录确认；
- `AUTO-TODO`：缺少针对性自动化测试，发布前必须补；
- `STATIC-TODO`：需要源码/依赖图静态审查，不能只靠运行测试；
- `MANUAL-TODO`：必须在 Mac 构建产物和受控环境/线上无损发布后人工验证。

| ID | 反例/风险 | 必须得到的结果 | 证据类型与当前占位 | 状态 |
|---|---|---|---|---|
| F-01 | A/B/C 三渠道，C 最近 TestChannel FAIL，连续 6 个新会话 | 首发仍为 A/B/C/A/B/C；C 没被移出公平环 | `internal/server/biz/unified_route_state_test.go::TestUnifiedRouteState_PrimaryFairnessIgnoresAvailability` | AUTO-EXISTS |
| F-02 | C 首发生产失败，A 救回 | 救援不推进首发游标；下个新会话仍按原序列 | `internal/server/orchestrator/unified_routing_test.go` 新增跨请求断言 | AUTO-TODO |
| F-03 | A 有 10 把 key，B 有 1 把 key | 外层长期首发份额仍为 1:1 | `internal/server/biz/unified_route_state_test.go` 增加多 key 用例 | AUTO-TODO |
| F-04 | 环运行中新增 ID 介于现有成员之间的 Channel，或删除 last 指向的 Channel | 按“下一个大于 last，否则首项”继续，不发生 modulo 漂移 | `internal/server/biz/unified_route_state_test.go` 成员变化用例 | AUTO-TODO |
| F-05 | 模型 X 与 Y 请求交错 | 两个模型游标互不推进 | `internal/server/biz/unified_route_state_test.go` 模型隔离用例 | AUTO-TODO |
| F-06 | 并发 600 个新会话争抢 A/B/C | 计数均匀且无 data race | `TestUnifiedRouteState_PrimaryFairnessIgnoresAvailability`；另需 `go test -race` | AUTO-EXISTS |
| F-07 | 捐赠过期/主动删除与 TestChannel FAIL 同时出现 | 前者退出 eligibility；后者保留公平首发 | candidate eligibility 回归 + 静态审查 `select_candidates.go` | STATIC-TODO |
| S-01 | 同一显式 session 连续多轮 | 保持成功 Channel，不重复推进公平游标 | `session_affinity_test.go::TestSelectCandidates_ExplicitTraceIsStickyAndCountedOnlyWhenNew` | AUTO-EXISTS |
| S-02 | 无 session 客户端携带相同开场前缀 | 用 HMAC 前缀保持亲和，不存原文 | `TestSessionAffinityRequestKey_StableContextPrefixAndIdentityScope`、`TestSelectCandidates_ContextPrefixAffinityKeepsChannelWithoutDoubleCounting` | AUTO-EXISTS |
| S-03 | 两个用户或项目有完全相同提示词 | 摘要不同，不能串 Channel 亲和 | `TestSessionAffinityRequestKey_StableContextPrefixAndIdentityScope` | AUTO-EXISTS |
| S-04 | 亲和目标被删除、过期或模型过滤 | 调用公平 `NextPrimary`，不能固定选排序第一项 | `session_affinity_test.go` 增加 missing-target 游标断言 | AUTO-TODO |
| S-05 | 亲和 A 失败，B 输出纯工具调用并救回 | 本次由 B 完成，随后会话重绑 B | `TestSessionAffinityRecording_BindsOnlySemanticSuccessAndRebindsAfterFailover`；需补端到端纯工具版本 | AUTO-EXISTS |
| S-06 | 同一未绑定会话并发发两次首轮请求 | 不应无意占用多个新会话位置，或必须明确为接受的限制 | 并发端到端测试 | AUTO-TODO |
| H-01 | 用户请求带非法 ID 导致生产 400，但精确 TestChannel PASS | 如实给用户生产错误；route 状态保持/更新为 1，不被生产 400 误杀 | orchestrator 程序化测试集成测试 | AUTO-TODO |
| H-02 | 401/402/429/5xx/TLS/无状态码错误分别发生 | 都走同一生产异常路径；无状态码白名单、无冷却、无自动禁用 | `rate_limit_tracking_test.go::{429,402}DoesNotCreateRoutingCooldown`、`channel_auto_disable_test.go::TestChannelService_ProductionFailureNeverExecutesLegacyAutoDisable`；其余静态审查 | AUTO-EXISTS |
| H-03 | 旧测试先开始、慢 FAIL；新测试后开始、快 PASS | 最终保持新 PASS，旧 FAIL 不得覆盖 | `unified_route_state_test.go` 增加 generation race 用例 | AUTO-TODO |
| H-04 | TestChannel panic、取消、超时或内部错误 | 不产生 Completed verdict，不覆盖旧状态 | `TestUnifiedRouteState_OnlyCompletedTestVerdictWrites`；需补 panic/timeout | AUTO-EXISTS |
| H-05 | 同一坏 route 被 50 个请求同时发现 | 只运行一个程序化精确测试，生产请求各自不等待 | `TestUnifiedRouteState_ProgrammaticTestsAreSingleflight` | AUTO-EXISTS |
| H-06 | Channel 同时支持 chat 和 Responses，只有 Responses 坏 | 测失败的 exact APIFormat，不能用 chat PASS 覆盖 | `candidates_basic_test.go` / `tester_test.go` 增加 ForcedAPIFormat 集成用例 | AUTO-TODO |
| H-07 | 同 Channel 两把 key，一把好一把坏 | verdict 和救援都精确到同一 credential；不得测试 key-A 后救援随机拿 key-B | `unified_routing_test.go::TestUnifiedRescue_UsesPassRoutesAndForcesExactCredential`；需补 tester 集成 | AUTO-EXISTS |
| H-08 | 同模型名映射到两个 ActualModel，一个拒模 | 分开测试/记录；错误 route 不污染另一个 ActualModel | exact route 集成测试 | AUTO-TODO |
| H-09 | 异步测试与下一次生产重试并发改 Source/API key | 两者使用独立 context 容器；race 测试无污染 | `internal/contexts` 克隆测试 + `go test -race` | AUTO-TODO |
| H-10 | 手工测试 PASS、程序化测试同输入却 FAIL（或相反） | 两个入口共用同一语义/terminal 判定器，差异只能来自明确的 exact route | `tester_test.go::{NonStreamOutputRequiresMeaningfulContent,StreamToolCallCountsAsHealthyOutput}` + 入口一致性测试 | AUTO-TODO |
| H-11 | 服务重启后所有 route 都 unknown | 不宣告全红；公平首发和 best-effort 正常，随后自动重建 verdict | 进程级测试 + UI 手工验证 | MANUAL-TODO |
| H-12 | endpoint/proxy 被 Owner 修改，旧 route 曾 PASS | 旧 verdict 失效，不能沿用到新网络路径 | 配置修订失效测试；当前 `RouteKey` 缺显式 revision | AUTO-TODO |
| H-13 | TestChannel 判定器自身有 bug，把实际可用的 A/B/C 全写成 FAIL | 生产首发仍轮到每个 Channel；救援仍遍历 distinct routes；不能形成全局拒绝服务，且生产成功与测试 FAIL 的矛盾可观测 | 全 FAIL 但生产成功的 orchestrator 集成测试 | AUTO-TODO |
| R-01 | 首发 FAIL，存在精确 PASS route | 下一次选择精确 PASS，包含正确 key/model/protocol | `TestUnifiedRescue_UsesPassRoutesAndForcesExactCredential` | AUTO-EXISTS |
| R-02 | 所有 route 都 FAIL/unknown | 先遍历所有 distinct exact routes，再在预算内重复；不能立刻“无可用渠道” | `TestUnifiedRescue_NoKnownPassWalksEveryRingSlot`；需补预算超过 route 数的重复用例 | AUTO-EXISTS |
| R-03 | 同 Channel 有两 key × 两 ActualModel | `AttemptedRoutes` 按 4 个精确 route 去重，不按 Channel 一次性跳过 | `unified_routing_test.go` 组合矩阵用例 | AUTO-TODO |
| R-04 | 初始无 PASS，重试中异步测试刚写入 PASS | 下一次选择重新读状态，优先新 PASS | 并发/可控时钟集成测试 | AUTO-TODO |
| R-05 | 已知 PASS 在到达最后一次前仍存在 | 最后一次在选择时必须为 PASS，不能落到 FAIL/unknown | pipeline + orchestrator 预算集成测试 | AUTO-TODO |
| R-06 | 已知 PASS 实际也瞬时失败，状态尚未异步更新 | 继续其他 PASS；若需重复仍有界，不永久封禁任何 route | 多 PASS 故障链测试 | AUTO-TODO |
| R-07 | Owner 设置 max retries=0 或关闭重试 | 只发一次并诚实报错；UI 明确无救援 | `retry-settings.test.mjs` + 手工文案检查 | AUTO-TODO |
| R-08 | 旧 API key profile/template 仍保存 weighted/adaptive 策略 | 生产忽略且 UI 无入口；最终清除僵尸字段/依赖 | `frontend/.../apikeys-unified-routing.test.mjs` + 后端依赖图静态审查 | STATIC-TODO |
| T-01 | 流在首个语义事件前出错/结束 | 不提交客户端，切 exact route 重试 | `pipeline_retry_test.go::TestPipeline_Process_StreamRetriesPreCommitError`、`empty_response_test.go::TestPreReadLlmStream_IncompleteTerminalBeforeContentRemainsRetryable` | AUTO-EXISTS |
| T-02 | 已给客户端文本后上游断流，无 terminal | 不重放；如实失败并触发 TestChannel，不能静默成功 | `TestPipeline_Process_StreamDoesNotRetryAfterContent`、`TestPreReadLlmStream_IncompleteAfterContentIsNotReplayed`；需补 observer 触发断言 | AUTO-TODO |
| T-03 | 上游只返回纯工具调用，无普通文本 | 视为有效语义输出和 TestChannel PASS | `tester_test.go::TestChannelStreamToolCallCountsAsHealthyOutput` + pipeline content tests | AUTO-EXISTS |
| T-04 | Responses 发 `[DONE]` 但没有 `response.completed` | 失败，不记成功，提交前则可救援 | `outbound_test.go::TestIsSuccessfulOutboundTerminalStreamEvent`、Responses terminal tests | AUTO-EXISTS |
| T-05 | Responses `response.incomplete`/`response.failed` 后又出现 completed 噪声 | 保持失败/不完整，不能被后续事件翻成成功 | `responses/aggregator_test.go`、`responses/outbound_stream_test.go` | AUTO-EXISTS |
| T-06 | 流最终成功，但 Close/客户端取消与 terminal 竞争 | outcome 只 finalize 一次，异步测试不读到下一 route | stream finalizer race test | AUTO-TODO |
| P-01 | 上游 message ID 为 `msg_abc`，经流/非流转换再发回 | 保留 `msg_abc` | `responses/inbound_stream_test.go::...PreservesProviderMessageID`、`outbound_stream_test.go::...PreservesProviderMessageID` | AUTO-EXISTS |
| P-02 | 上游不给 ID，网关生成后客户端回放 | 生成可接受的 `msg_...`，绝不生成 message 类型的 `item_...` | `responses/inbound_stream_test.go::...GeneratesReplayableMessageID`、`outbound_convert_test.go` | AUTO-EXISTS |
| P-03 | Codex Lite 客户端未带或带错 reasoning.context | 最终上游请求强制 `all_turns`，含 pass-through | `codex/codex_simulator_test.go`、`override_test.go`、`pass_through_test.go` | AUTO-EXISTS |
| P-04 | 多个 message/refusal/reasoning item 交错 | ID 与 item 生命周期不串位，可在下一轮安全引用 | 现有 refusal/reasoning 测试；多 message 交错回归仍需补 | AUTO-TODO |
| U-01 | Owner 打开重试设置 | 只看到 enable、救援预算、两个 timeout；看不到策略/冷却/状态码/同 Channel 重试 | `frontend/.../retry-settings.test.mjs` | AUTO-EXISTS |
| U-02 | 普通用户查看渠道卡片 | route 状态、模型、协议、测试时间和脱敏错误诚实；unknown 不伪装 FAIL | `channel-availability.test.mjs`；当前卡片聚合语义需静态复核 | STATIC-TODO |
| U-03 | 同 Channel 一把 key PASS、另一把 FAIL | UI 不得用“最近一条绿色”声称整个 Channel 可用 | route 展开/聚合测试 | AUTO-TODO |
| U-04 | 一次 API 请求先失败两次后被第三 route 救回 | 用户看到完整 attempt chain、最终救援 Channel 和实际错误 | `api-key-activity-panel.test.mjs` + API 数据集成测试 | AUTO-EXISTS |
| U-05 | 上游错误包含 Authorization/token/正文片段 | 保留状态码与安全错误原意，但彻底脱敏密钥和敏感正文 | `tester_test.go::TestChannelHTTPErrorNeverReturnsStructuredSecretsOrRawBody` + 手工样本 | AUTO-EXISTS |
| U-06 | Owner 选择 hidden/custom upstream error | 不得破坏“用户本人看到诚实排障错误”的产品约束 | 当前 UI 仍有此入口，需产品/静态修正 | STATIC-TODO |
| Q-01 | 程序化或手工 TestChannel 成功消耗 token | 不进用户额度、钱包回馈、排行榜；只影响 route verdict | SourceTest 计量集成测试 + 数据库查询 | AUTO-TODO |
| Q-02 | 首发失败、第二 route 成功 | 只按既有规则结算真实生产执行；诊断测试与失败尝试不重复发放钱包回馈 | request execution / wallet 集成测试 | AUTO-TODO |
| Z-01 | 旧 LoadBalancer、评分/配额 selector 仍可由生产 DI 到达 | 生产依赖图只能到 unified path；删除不可达字段/分支，测试工具若保留需明确隔离 | `select_candidates.go`、FX 构造和全仓静态调用图 | STATIC-TODO |

---

## 10. 已关闭的阻断项与明确边界

本轮已经关闭以下发布阻断项：

1. route identity 已包含 endpoint/proxy 配置修订和统一的结构化凭据身份；旧配置 PASS 无法授权新配置。
2. TestChannel 采用 generation + singleflight；旧慢结果不能覆盖新结果，异常结束不写 verdict。
3. 程序化测试强制 exact credential、ActualModel、APIFormat 与配置修订，并以 one-shot 模式禁止借全局重试换 key/模型/协议。
4. TestChannel 流必须同时具备语义输出和成功 terminal；文本/纯工具调用后 EOF、incomplete/failed、流错误或 Close 错误都判 FAIL。
5. 非流与流共享唯一 terminal 判定器；Responses incomplete/failed/canceled 即使带部分内容也不能误记成功或绑定会话。
6. 渠道卡片枚举当前配置的全部 exact routes，未测试项明确为 unknown；一个绿色 route 不再冒充整条渠道全绿。
7. 用户 API 活动展示每次 execution、最终救援渠道、真实 HTTP 状态和脱敏错误；旧 hidden/custom 设置不再能隐藏上游错误。
8. 个人 API Profile 仅提交模型映射、模型限制与额度，不携带共享渠道或负载均衡字段，既不绕过公平路由也不阻断正常保存。
9. SourceTest 已从日/周额度、钱包、捐赠回馈、校内排行及通用 Owner Dashboard 的请求/token/成本/吞吐统计中排除。
10. 生产依赖图已切断旧 LoadBalancer、评分、配额优先、冷却、自动禁用、状态码重试和同渠道重试入口；Owner UI 只保留统一策略允许的四个配置。

以下是刻意保留并必须让维护者知道的边界：

- availability 与公平游标是进程内状态；服务重启后 route 回到 unknown，但不会变成 FAIL，也不会阻止公平首发。当前生产为单实例；若扩成多实例，必须先设计共享游标和 verdict 一致性。
- 同一“尚未绑定”的无 session 会话若真正并发发出多次首轮请求，可能占用多个公平位置；成功后会收敛到同一亲和绑定。该并发边界不影响已绑定会话。
- 流已经向客户端提交语义内容后不能安全地换渠道重放；系统会如实失败并保留排障元数据，避免重复文本或工具副作用。
- 多 route 全部耗尽时，客户端收到 `upstream_candidates_exhausted` 聚合错误，其中保留尝试数量、最后 HTTP 状态和脱敏后的最后上游错误；每次具体失败可在本人 API 活动中展开查看。
- 仓库仍有不再接入生产的旧策略实现，便于上游兼容测试；它们不是运行时裁决者。后续清理必须以删除为主，并保持统一路由依赖图测试。

---

## 11. 实施结果

1. **状态层**：稳定 Channel 游标、独立救援游标、exact route verdict、singleflight、generation 与安全凭据/config identity 已落地。
2. **选择层**：生产只走统一外层环；会话亲和、HMAC 上下文前缀、精确 `AttemptedRoutes`、PASS 优先和全 unknown/FAIL best-effort 使用同一规则。
3. **测试闭环**：生产异常只触发异步 exact TestChannel，不直接写可用性；手工与程序测试共用 verdict 判定器，测试流量保持 `SourceTest`。
4. **协议层**：提交前允许透明救援，提交后禁止重放；普通文本和纯工具调用均有效，但必须有协议成功 terminal。
5. **Responses**：保留/生成可回放 `msg_...`，覆盖多 message/refusal 交错，并强制 Codex Lite `reasoning.context=all_turns`。
6. **UI**：Owner 只配置重试开关、救援预算和两个 timeout；用户可查看 exact route 可用性、模型/协议、测试时间、脱敏错误及完整 attempt chain。
7. **计量**：TestChannel 不进入用户额度、钱包、捐赠回馈或任何排行/统计；生产用量规则保持不变。
8. **发布边界**：产物只允许在 Mac 从干净的 exact commit 构建；腾讯云只加载产物并单独重建 AxonHub，禁止构建、禁止重启 FRP/代理/无关服务。

---

## 12. 验证与发布记录

目标版本：`v1.0.0-beta5-ucas.9`。

自动验证基线：

- 根模块：`go test ./...`
- LLM 子模块：`cd llm && go test ./...`
- 关键并发：contexts、unified route state、orchestrator exact TestChannel/affinity/terminal、pipeline 均执行目标 `go test -race`
- 前端：17 个统一路由/活动/可用性/引导/设置测试、`tsc --noEmit`、目标 ESLint、正式 `vite build`
- 格式与差异：Go `gofmt`、前端 Prettier、`git diff --check`

生产发布完成后，在本节追加 exact commit、PR、tag、镜像摘要、数据库备份/完整性、根页面及全部静态资源、未认证 `/v1/models=401`、认证 Responses terminal、容器重启计数和受保护服务 PID。本文不以“代码存在”代替运行证据。
