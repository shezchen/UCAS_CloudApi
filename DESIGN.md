# 果壳云中转 — 控制面设计计划

> 本文档是后续开发的唯一基准。实现前先读本文；若实现与本文冲突，先更新本文再改代码。
>
> 状态：已确认架构方向，部分运营参数未拍板（见文末「开放问题」）。
> 最后更新：2026-07-20

---

## 1. 一句话定位

在**已经跑通的 AxonHub** 之上补一层面向群友的控制面：

- 国科大邮箱认证
- 每人独立 API Key
- 配额 / 并发 / 撤销 / 审计
- 基于 AxonHub 日志的用量与排行榜
- 捐赠渠道的登记与治理（二期）

**不是**重新实现 API 转发核心，**不是**公开免费机场。

定位表述：

> 群友公益、受限访问的模型额度互助池；主要用于个人 Plan 用尽或临时任务补救，也允许捐赠者共享闲置额度。

---

## 2. 环境与职责划分

| 环境 | 职责 |
|------|------|
| 开发机（当前） | 写控制面代码、本地运行与联调 |
| 生产服务端 | 已部署可运行的 AxonHub；未来部署本控制面，与 AxonHub 同机或内网通信 |

### 2.1 原则

1. **保留 AxonHub 作为数据面**，不重写转发、负载、上游协议适配。
2. **控制面独立进程**，用 Node 实现；生产与 AxonHub 并列部署。
3. **账户数据自有库**，不修改 AxonHub 表结构；统计优先读 AxonHub 可信日志。
4. **用户流量走控制面网关**（见 §4），便于限额、审计、防外传。
5. 密钥与凭据只进环境变量 / 密钥管理，**禁止进 Git**。

### 2.2 开发机现状注意

根目录若存在 `*.pem`、测试 Key、管理员账密、截图等，一律视为本地私密文件：

- 加入 `.gitignore`
- 不提交、不写进文档正文
- 群内已暴露过的凭据按已泄露处理，上线前全部轮换

---

## 3. 目标架构

```text
客户端 (OpenCode / 其他 OpenAI 兼容客户端)
       │
       │  Authorization: Bearer <用户独立 Key>
       ▼
┌──────────────────────────────────────┐
│  控制面 (Node)                        │
│  - 鉴权 / 配额 / 并发 / 审计           │
│  - 邮箱认证与账户                      │
│  - 用量查询 / 排行榜 API               │
│  - (二期) 捐赠渠道登记                 │
└──────────────┬───────────────────────┘
               │ 内网转发 (服务端 Key 或映射)
               ▼
         AxonHub (已有)
               │
               ▼
     捐赠的上游渠道 / Plan
     GPT / Qwen / GLM / Kimi ...

AxonHub 请求日志 ──只读──► 控制面统计与排行榜
```

### 3.1 流量模式（已选）

**A：客户端 → 控制面 → AxonHub**

理由：一人一 Key 的限额、并发、撤销、审计都在控制面闭环；AxonHub 侧可用服务账号或受控 Key 池，不把上游管理能力暴露给终端用户。

备选 B（客户端直打 AxonHub，控制面只发 Key）**不做**，除非本文明确修订。

---

## 4. 技术栈（已定）

| 层 | 选型 | 说明 |
|----|------|------|
| 运行时 | Node.js 22 LTS | |
| 语言 | TypeScript | strict |
| HTTP | Hono | 轻量，适合网关 + REST |
| 校验 | Zod | 请求/响应/环境变量 |
| 账户库 | PostgreSQL | 用户、Key、配额、审计 |
| 缓存/限流 | Redis | 验证码、并发计数；一期可先内存实现，接口预留 |
| 邮件 | SMTP | 国科大邮箱验证码 |
| 包管理 | pnpm | |
| 部署 | Docker Compose 优先 | 与 AxonHub 同机时用内网 DNS/host 网络 |

### 4.1 建议仓库结构

```text
/
├── DESIGN.md                 # 本文件（基准）
├── README.md                 # 运维与本地启动（实现时补）
├── .gitignore
├── docker-compose.yml        # 本地：api + postgres (+ redis)
├── apps/
│   └── api/
│       ├── package.json
│       ├── tsconfig.json
│       ├── src/
│       │   ├── index.ts              # 入口
│       │   ├── env.ts                # 环境变量 (Zod)
│       │   ├── db/                   # PG schema / migrate / repos
│       │   ├── modules/
│       │   │   ├── auth/             # 邮箱验证码登录
│       │   │   ├── keys/             # 用户 API Key CRUD / 撤销
│       │   │   ├── gateway/          # OpenAI 兼容代理 → AxonHub
│       │   │   ├── quota/            # 配额与并发
│       │   │   ├── stats/            # 用量 / 排行榜（读 AxonHub 日志）
│       │   │   └── admin/            # 管理接口（后期）
│       │   └── lib/                  # 邮件、加密、错误类型
│       └── Dockerfile
└── packages/
    └── shared/               # 可选：公共类型；人少时可先不拆
```

一期允许单包 `apps/api` 起步，不必强上 monorepo 工具；若用 pnpm workspace，保持简单即可。

---

## 5. 分期范围

### 5.1 一期（必须交付）

1. **邮箱认证 + 账户库**
   - 限制国科大邮箱后缀（具体后缀列表可配置，如 `*.ucas.ac.cn` 等）
   - 发送限时验证码，验证成功建立会话/账户
   - 初期**无**人工审核、**无**邀请码

2. **独立 API Key**
   - 每用户至少一把 Key，允许用户为不同客户端创建多把 Key
   - Key 与用户绑定；支持撤销、停用
   - 用户可单独设置自己每把 Key 的日上限，但无权修改账户日总上限
   - 控制面鉴权后转发 AxonHub
   - 不限量 Key 仅管理员线下审批后标记（数据模型预留 `tier` / `unlimited`）

3. **配额与基础治理**
   - 每用户日总上限默认 **200,000,000 tokens（200M）**，仅项目所有者可修改
   - 每账户最多 **4 个并发请求**，该用户所有 API Key 合并计算
   - 单 Key 日上限由该用户自行设置；实际可用量取单 Key 剩余额度与账户总剩余额度的较小值
   - 达到总上限时必须拒绝或终止请求并返回明确错误，不允许静默超额
   - 请求审计日志（谁、何时、模型、状态、token 若可得）

4. **用量统计与排行榜**
   - 数据源：**AxonHub 请求日志（只读）**，禁止客户端上报 token
   - 个人：今日 / 近 24h / 本周 / 近 7 日 / 累计
   - 全局与排行榜（可按 token 或估算成本）
   - 模型维度聚合（AxonHub 日志字段足够时）

5. **安全基线**
   - 生产关闭公开注册以外的管理面暴露
   - 管理接口单独鉴权
   - 速率限制（验证码发送、登录、转发）
   - 文档与脚本中不出现真实密钥

### 5.2 二期（一期稳定后再做）

- 捐赠渠道登记页：捐赠者、渠道元数据、有效日期、状态；**不展示上游凭据给其他普通用户**
- 租户隔离：普通用户只能查看和管理自己捐赠的渠道，看不到、也不能操作他人的渠道
- 项目所有者拥有全局视图与管理权限；但不得随意删除捐赠渠道，只能停用或调整运行状态
- 渠道仅可由捐赠者主动删除，或在到达有效日期后自动过期；删除采用软删除，保留审计记录
- 渠道健康：余额/错误率/停用；不可用或耗尽的渠道退出调度，但记录不删除
- 多渠道自动轮询 / 故障转移：先确认并复用 AxonHub 已有的可用性选择能力；仅在其不支持时由控制面补充，避免重复实现
- 用户规模约 50 人后：邀请制或管理员审核
- 成本「原价估算」展示优化、缓存命中等细项（若日志支持）

### 5.3 明确不做（除非修订本文）

- 重写 AxonHub 转发核心
- 公开免费、无认证的共享 Key
- 客户端自报用量
- 共享管理员账户给群友
- 在控制面重复实现上游协议细节（OpenAI 兼容透传即可）

---

## 6. 核心领域模型

### 6.1 用户 `users`

| 字段 | 说明 |
|------|------|
| id | UUID |
| email | 国科大邮箱，唯一 |
| status | `active` / `disabled` |
| role | `user` / `owner`；`owner` 为项目所有者，拥有全局管理权限 |
| tier | `default` / `approved_unlimited` 等 |
| daily_token_limit | 默认 200M；仅项目所有者可修改 |
| created_at | |
| last_login_at | |

### 6.2 邮箱验证码 `email_codes`

| 字段 | 说明 |
|------|------|
| email | |
| code_hash | 只存哈希 |
| expires_at | 短时（如 10 分钟） |
| consumed_at | |
| request_ip | |
| created_at | |

约束：同一邮箱发送频率限制；同一 IP 频率限制。

### 6.3 API Key `api_keys`

| 字段 | 说明 |
|------|------|
| id | UUID |
| user_id | FK |
| key_prefix | 可展示前缀，如 `gk_xxxx` |
| key_hash | 只存哈希，明文仅创建时返回一次 |
| name | 可选备注 |
| status | `active` / `revoked` / `disabled` |
| daily_token_limit | 用户可设置的单 Key 日上限；可空表示仅受账户日总上限约束 |
| created_at | |
| revoked_at | |
| last_used_at | |

### 6.4 会话（若采用 Bearer session 管控制台）

控制台登录可用 HTTP-only Cookie 或短期 JWT；**转发用的 API Key 与控制台 session 分离**。

### 6.5 审计 `audit_logs` / 网关请求日志

控制面自己记一份网关访问日志（user_id, key_id, path, model, status, latency, upstream_request_id…），与 AxonHub 日志互补：

- 鉴权失败、配额拒绝：以控制面为准
- Token 计量、上游渠道：以 AxonHub 为准

### 6.6 配额配置

默认值由环境变量提供，用户级总额度由数据库保存并仅允许项目所有者覆盖：

- `DEFAULT_DAILY_TOKEN_LIMIT=200000000`
- `DEFAULT_CONCURRENT_LIMIT=4`
- `DEFAULT_MAX_BODY_TOKENS`（可选）

强制规则：

1. 并发按 `user_id` 聚合，该用户所有 Key 合计最多 4 个在途请求，不按 Key 分摊出额外并发。
2. 账户日总额度默认 200M，仅 `role=owner` 可修改；普通用户和 API Key 接口均不得改动。
3. 用户可修改自己名下每把 Key 的日上限，不能修改他人 Key；设置值高于账户日总额度不会扩大账户额度。
4. 单次请求的有效剩余额度为 `min(Key 剩余额度（若设置）, 账户总剩余额度)`。
5. 日窗口口径统一按服务端配置时区的自然日计算，并在 API 响应中返回重置时间。

### 6.7 捐赠渠道 `donated_channels`（二期）

| 字段 | 说明 |
|------|------|
| id | UUID |
| owner_user_id | 捐赠者用户 ID，所有权与访问控制依据 |
| name | 捐赠者可见的渠道名称 |
| credentials_encrypted | 加密保存的上游凭据，不对其他普通用户返回 |
| valid_until | 捐赠者设置的有效日期，必填 |
| status | `active` / `unavailable` / `disabled` / `expired` / `deleted` |
| deleted_at | 软删除时间 |
| created_at / updated_at | |

授权规则必须在服务端查询层执行，不能只依赖前端隐藏：

- `role=user`：查询必须带 `owner_user_id = current_user.id`，仅能管理自己的渠道。
- `role=owner`：可查看和管理全局资源，包括停用、恢复和健康状态；不能绕过删除规则直接硬删除。
- 删除仅接受捐赠者对自己渠道发起，或过期任务在 `valid_until` 到达后执行；均转为软删除/过期状态并保留审计。
- 到期前系统不得因暂时不可用、余额耗尽等原因删除记录，只能将其移出调度。

---

## 7. API 草案

前缀建议：`/api/v1`。网关走 OpenAI 兼容路径，与统计 API 分离。

### 7.1 认证

- `POST /api/v1/auth/request-code`  
  body: `{ email }`  
  校验后缀 → 发码 → 通用成功响应（防枚举可统一文案）

- `POST /api/v1/auth/verify`  
  body: `{ email, code }`  
  成功：建立控制台 session，返回用户信息

- `POST /api/v1/auth/logout`

### 7.2 用户与 Key

- `GET /api/v1/me`
- `GET /api/v1/keys`
- `POST /api/v1/keys`
- `PATCH /api/v1/keys/:id/quota`（仅 Key 所有者可设置该 Key 日上限）
- `POST /api/v1/keys/:id/revoke`

### 7.3 用量

- `GET /api/v1/stats/me?range=today|24h|7d|week|all`
- `GET /api/v1/stats/me/models?range=...`
- `GET /api/v1/stats/leaderboard?range=...&limit=20`
- `GET /api/v1/stats/global?range=...`

统计实现：定时或请求时查询 AxonHub 日志存储（PG/API，以生产实际为准），按用户 Key 映射聚合。

### 7.4 OpenAI 兼容网关

- `POST /v1/chat/completions`（及后续需要的 `/v1/models` 等）
- Header: `Authorization: Bearer <user_api_key>`
- 流程：
  1. 校验 Key → 用户状态
  2. 按账户聚合检查 4 并发、单 Key 日额度与账户 200M 日总额度
  3. 转发 AxonHub（注入上游凭据，**不要**把用户 Key 原样传给 AxonHub，除非采用 1:1 映射方案）
  4. 流式响应透传（SSE）；达到硬额度时终止并返回明确的 quota 错误
  5. 记录审计；token 以 AxonHub 日志回写或响应 usage 为准做配额扣减策略（见 §8）

### 7.5 管理（一期最小）

- 管理员身份：环境变量 `ADMIN_EMAILS` 或独立 admin token
- `POST /api/v1/admin/users/:id/disable`
- `POST /api/v1/admin/keys/:id/revoke`
- `PATCH /api/v1/admin/users/:id/tier`
- `PATCH /api/v1/admin/users/:id/quota`（仅项目所有者可修改账户日总上限）

管理接口不得对公网无鉴权暴露。

---

## 8. 与 AxonHub 的集成要点

### 8.1 转发

- 配置：`AXONHUB_BASE_URL`、`AXONHUB_API_KEY`（服务账号）或 Key 池
- 超时、流式、错误码透传策略要明确：上游 429/5xx 映射为对客户端友好错误，并记日志
- **推荐**：控制面用户 Key 与 AxonHub Key **解耦**（多用户共享受控上游 Key，靠控制面配额隔离）。若 AxonHub 支持按 Key 精细日志，可改为为每用户在 AxonHub 创建子 Key 并存储映射——二选一在实现网关前写进本文修订。

**一期默认决策：解耦 + 控制面鉴权配额；AxonHub 用服务 Key。**  
日志关联：控制面生成 `x-request-id`，尽量传给上游或自行双写，便于对齐统计。

### 8.2 统计数据源

1. 查清生产 AxonHub 日志落点（DB 表 / 导出 API / 文件）
2. 只读账号访问
3. 用控制面 `user_id`↔`key_id`↔请求 id 对齐
4. 若一期无法稳定对齐 token，则：
   - 网关层用响应 `usage` 做配额（准实时）
   - 排行榜异步对账 AxonHub（准最终一致）
   - 在 README 标明以谁为准

### 8.3 硬额度与流式截断

200M 是账户级硬上限，不能把响应结束后的统计当作唯一拦截手段而允许静默超额：

1. 请求前原子检查账户与 Key 的剩余额度，并原子占用一个账户并发槽。
2. 若 AxonHub 能提供流式实时 token 计量，达到有效剩余额度时发送标准 quota 错误并关闭流。
3. 若只能在响应结束后得到准确 usage，则请求前按输入估算量与声明的最大输出 token 做保守预留；预留超过剩余额度时直接拒绝。实现不得宣称无法保证的“精确逐 token 截断”。
4. 请求结束、失败或断连均须原子释放并发槽，并按最终 usage 结算/返还预留额度。
5. 配额扣减和并发控制需要跨进程一致；生产使用 Redis 或数据库原子操作，不能依赖单进程内存计数。

### 8.4 渠道可用性与轮询

- 实现渠道调度前，先验证当前 AxonHub 版本是否支持健康检测、可用性选择、权重或故障转移，并记录验证结果。
- AxonHub 已支持时，以 AxonHub 为唯一调度执行方；控制面只维护所有权、有效期、启停和展示状态。
- AxonHub 不支持所需能力时，另行修订本文后补最小调度逻辑，禁止未经验证重复造轮询系统。
- 临时故障、余额耗尽或健康检查失败只让渠道退出轮询，不删除捐赠记录；恢复可用后可重新进入。

### 8.5 不要做的事

- 不在 AxonHub 库里建控制面表
- 不把 AxonHub 管理员密码交给终端用户
- 不在群里发生产 Key

---

## 9. 安全要求

1. **上线前**：轮换所有曾在群内出现的 AxonHub Key、上游 Key、管理员密码。
2. API Key 只存哈希；验证码只存哈希。
3. 验证码与登录接口严格限流。
4. CORS 按实际控制台域名收紧。
5. 生产 HTTPS 终止在反代（Caddy/Nginx）；Node 监听本机或内网。
6. 法律/合规：上游套餐是否允许二次分享 — **扩散前必须确认**（见开放问题）；产品文案避免鼓励倒卖。
7. 违规（外传、倒卖、打满滥用）：撤销 Key、停用账户；不影响他人。

---

## 10. 实现顺序（给 Agent 的任务序列）

按序执行，未完成前一阶段不要跳做二期功能。

### Phase 0 — 仓库与工程骨架

- [ ] `.gitignore`（node_modules、.env、`*.pem`、`*key*`、截图、本地笔记等）
- [ ] `apps/api` 初始化：Hono + TS + Zod + 脚本
- [ ] `env` 示例文件 `.env.example`（无真实密钥）
- [ ] 健康检查 `GET /health`
- [ ] 本地 Docker Compose：Postgres

### Phase 1 — 账户与邮箱认证

- [ ] users / email_codes 表与迁移
- [ ] 邮件发送抽象（dev 可打日志，prod 走 SMTP）
- [ ] request-code / verify / session
- [ ] 邮箱后缀白名单配置

### Phase 2 — API Key

- [ ] api_keys 表
- [ ] 创建（明文一次性返回）/ 列表（仅前缀）/ 撤销
- [ ] Key 鉴权中间件

### Phase 3 — 网关转发

- [ ] `/v1/*` 代理到 AxonHub
- [ ] 流式透传
- [ ] 审计日志
- [ ] 基础错误映射
- [ ] 本地用 AxonHub 测试实例或 SSH 隧道联调说明写入 README

### Phase 4 — 配额

- [ ] 账户日总额度默认 200M，仅项目所有者可修改
- [ ] 用户可设置自己的单 Key 日上限，并与账户总额度取较小剩余值
- [ ] 每账户所有 Key 合计 4 并发，生产使用跨进程原子计数
- [ ] 流式额度预留、结算及可用时的硬截断
- [ ] 超限错误响应格式统一

### Phase 5 — 统计

- [ ] 对接 AxonHub 日志只读方案（先 spiking 再写死）
- [ ] 个人用量 / 全局 / 排行榜 API
- [ ] 与网关 usage 对账策略

### Phase 6 — 硬化与部署

- [ ] Dockerfile + 生产 compose 片段
- [ ] 管理接口最小集
- [ ] 限流、日志、优雅退出
- [ ] README：部署到「已有 AxonHub 的服务端」步骤
- [ ] 上线检查清单：凭据轮换、关闭默认共享、备份

### Phase 7 — 二期（另开里程碑）

- [ ] 捐赠渠道所有权、有效日期、软删除模型与服务端行级授权
- [ ] 普通用户仅管理自己的捐赠；项目所有者管理全局资源
- [ ] 核查并优先复用 AxonHub 健康检查与可用性轮询
- [ ] 到期任务：自动退出调度并标记过期，不提前删除
- [ ] 审核 / 邀请

---

## 11. 配置项清单（`.env.example` 应包含）

```text
NODE_ENV=development
PORT=3000

DATABASE_URL=postgres://...

# 邮箱
SMTP_HOST=
SMTP_PORT=
SMTP_USER=
SMTP_PASS=
SMTP_FROM=
EMAIL_SUFFIX_ALLOWLIST=ucas.ac.cn

# 会话
SESSION_SECRET=

# AxonHub
AXONHUB_BASE_URL=http://127.0.0.1:xxxx
AXONHUB_API_KEY=

# 配额默认
DEFAULT_DAILY_TOKEN_LIMIT=200000000
DEFAULT_CONCURRENT_LIMIT=4
QUOTA_TIMEZONE=Asia/Shanghai

# 管理
ADMIN_EMAILS=admin@example.ac.cn

# Redis（可选）
REDIS_URL=
```

---

## 12. 验收标准（一期）

1. 非白名单邮箱无法拿到码/注册。
2. 用户验证后可创建 Key；Key 可调用网关访问模型（经 AxonHub）。
3. 撤销 Key 后立刻 401，不影响其他用户。
4. 同一账户所有 Key 合计最多 4 个在途请求；第 5 个返回明确的并发限制错误。
5. 用户可修改自己的单 Key 日限额，但不能修改账户 200M 日总额度；项目所有者可修改账户总额度。
6. 请求不能静默突破账户日总上限：可实时计量时截断报错，否则采用保守预留并在请求前拒绝。
7. 普通用户无法查询或操作他人的捐赠渠道；项目所有者可管理全局资源。
8. 捐赠渠道在捐赠者删除或有效期到达前不会被删除；不可用只退出调度。
9. 个人用量与排行榜有数据，且来源不是客户端上报。
10. 仓库内无真实密钥；`.env.example` 可复制启动。
11. 生产部署文档可让第二人按文档上线。

---

## 13. 开放问题（未拍板，实现用配置占位）

| 问题 | 建议占位 | 谁决定 |
|------|----------|--------|
| 单次最大 token、请求时长 | env 小默认值 | 开发建议 + 群共识 |
| 何时上管理员审核 | 约 50 人 | 运营 |
| 捐赠者是否直连 AxonHub 控制台 | 否，走控制面登记 | 默认否 |
| AxonHub 日志具体表结构/API | Phase 5 spike | 开发查生产 |
| AxonHub 是否原生支持健康选择/轮询 | Phase 7 前验证，优先复用 | 开发查当前版本 |
| 模型统一命名 | 先透传 AxonHub 名称 | 后期 |
| 安全与运维负责人 | 未定 | 群内指定 |
| 上游套餐是否允许二次分享 | 上线扩散前确认 | 合规/捐赠者 |

开放问题**不得**阻塞 Phase 0–3；用配置与 TODO 标记即可。阻塞上线的是：凭据轮换、日志对接方案与硬额度执行方案验证。

---

## 14. 给后续 Agent 的硬约束

1. **先读本文再写代码**；变更架构必须先改 `DESIGN.md`。
2. **不重写 AxonHub**；不提交密钥与 pem。
3. **不替用户做未文档化的扩大范围**（如突然上 OAuth、微服务拆分）。
4. 一期优先：**能跑通的鉴权网关 + Key + 配额 + 基础统计**。
5. 统计与配额以服务端为准；流式场景注意 usage 与超时。
6. 代码需要可在 Windows 开发机开发、Linux 服务端部署。
7. 用户未明确要求时，不删除根目录现有本地私密文件，只保证 git 忽略。
8. 所有资源查询和写操作必须在服务端做所有权校验；前端隐藏不算权限隔离。
9. 账户总额度默认 200M 且仅项目所有者可改；并发按账户聚合为 4，禁止按 Key 放大。
10. 未验证 AxonHub 的现有能力前，不自行实现渠道轮询。

---

## 15. 文档修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-20 | 初版：基于群讨论汇总与开发环境确认（Node/Hono、流量模式 A、AxonHub 已有） |
| 2026-07-20 | 补充硬约束：捐赠所有权与有效期、项目所有者全局权限、账户 4 并发、默认 200M 日总额度、单 Key 子额度及 AxonHub 轮询复用原则 |
