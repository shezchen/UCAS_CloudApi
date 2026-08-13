# UCAS Campus Sharing Gateway Maintenance Handbook

> This document covers the campus-sharing extensions in `shezchen/UCAS_CloudApi`. Refer to the other documentation in this repository for general upstream AxonHub behavior.
>
> Last audited: 2026-08-06. Branches, commits, images, and production processes can drift. Always inspect the actual Git and runtime state before maintenance.

## 1. Purpose and non-negotiable principles

This is a student-maintained, nonprofit shared gateway for the UCAS community. Routine work should be self-service: users consume the gateway, contributors maintain their donations, and the Owner controls global policy.

The following are acceptance constraints:

- Members can view and manage only their own API keys and the credentials, configuration, expiry, and model overrides of their own donated channels.
- Members cannot read or change another person's channel credentials. Every project member can still see a channel's name, provider, description, contributor, expiry, model count, and health. This provides transparency and credits contributors.
- The Owner can manage global resources but should not become the manual operator for model catalogs, channel health, or routine donation maintenance. Only the contributor may change or retain the donation expiry, and only the contributor may delete an active donated channel; the Owner cannot do either on the contributor's behalf.
- Product copy must say “Donate channel”, not the easily misunderstood “Add channel”.
- The primary onboarding path is “Use”: see legal model IDs, create an API key, and copy the compatible endpoint. Donation is a separate, prominent contribution path.
- A donated channel is not deleted or permanently disabled because of a transient failure before its expiry. It leaves permanently only when the contributor deletes or disables it, or it expires.
- Fairness means best-effort rotation among healthy candidates, not mechanically forcing identical token totals.
- Production does not retain prompts, response bodies, or stream chunks. Short-lived diagnostics retain only sanitized error summaries.
- Build on the Mac or another trusted builder. The server receives an already verified `linux/amd64` artifact.
- A release replaces AxonHub only. Never restart the host, Docker daemon, HAProxy, sing-box, FRP, HCZ, Matrix, or unrelated services.

## 2. System overview

```text
UCAS email registration
  → authenticated project member
  → personal API key
  → OpenAI / Anthropic / Gemini compatible endpoint
  → API-key quota + account daily/weekly quota
  → model access check
  → session affinity + healthy candidates + best-effort fair rotation
  → project-owned or student-donated channel
  → normalized response, effective-token accounting, rankings, wallet settlement
```

The admin plane uses JWT plus project context; model calls use the user's API key. HTTP routes are registered in `internal/server/routes.go`, and GraphQL schemas live in `internal/server/gql/`.

## 3. Identity, authorization, and public projections

| Object or action | Member | Channel contributor | Owner |
|---|---:|---:|---:|
| View legal model IDs and capabilities | Yes | Yes | Yes |
| View channel source, description, contributor, and health | Yes | Yes | Yes |
| View the provider-quota battery | Read-only | Read-only | Yes |
| Create and manage own API keys | Yes | Yes | Yes |
| View own API activity and error summaries | Yes | Yes | Yes |
| Read another person's API key or channel credentials | No | No | Yes |
| Change a donated channel | No | Own only | Global management, but cannot change expiry or delete an active donation |
| Change channel-model capabilities | No | Own channel overrides only | Maintain global models on the advanced model page; contributor override API is forbidden |
| Change global daily/weekly quotas or friend links | No | No | Yes |
| Refresh or reset provider quotas | No | No | Yes |

A valid nickname is preferred in rankings and contributor attribution. Otherwise, the server uses a stable, project-scoped `同学-xxxxxxxx` alias that does not disclose the database user ID.

API-key names are user-defined labels and have no global or per-user uniqueness check. One user may create multiple keys with the same name and distinguish them by the last four characters.

Key implementation areas:

- Campus identity and public aliases: `internal/server/biz/user.go`, `internal/server/biz/campus_identity.go`
- API-key ownership: `internal/server/biz/api_key.go`
- Channel privacy and contributor authorization: `internal/server/biz/channel.go`, `internal/server/biz/campus_catalog.go`
- GraphQL authorization: `internal/authz/`, `internal/server/gql/*resolvers.go`

## 4. Registration and email verification

Public registration accepts only these exact domains, not subdomains or look-alike suffixes:

- `@mails.ucas.ac.cn`
- `@ucas.ac.cn`
- `@mails.ucas.edu.cn`
- `@ucas.edu.cn`

The client first calls `POST /admin/auth/signup/verification` for a six-digit code and then `POST /admin/auth/signup`. Public signup always creates a non-Owner member and never replaces the existing Owner identity.

### 4.1 Password recovery

Password recovery uses two public endpoints:

1. `POST /admin/auth/password-reset/verification` accepts `email` and, when the request is accepted, returns `202`, `challengeToken`, and `resendAfterSeconds`.
2. `POST /admin/auth/password-reset` accepts that same email together with `challengeToken`, the six-digit `verificationCode`, and `newPassword`.

Recovery applies to every existing activated account email and is not restricted to the campus signup domains. This includes the existing Owner's QQ mailbox. Addresses are trimmed and normalized to lowercase; public signup itself remains restricted to the four UCAS domains above.

The verification endpoint returns the same `202` status, message, and response fields for existing and unknown addresses, so neither the HTTP response nor the UI reveals account existence. Only an existing activated account receives a message; account lookup and SMTP delivery run outside the public request path. Signup and password recovery use separate `registration` and `password_reset` purposes with separate digest domains, so codes cannot cross purposes. A recovery code is bounded by expiry, attempt count, and one-time consumption; a successful reset also invalidates every other outstanding recovery challenge for that email.

A successful reset increments the account's `auth_version`, immediately revoking every previously issued browser JWT. The frontend must clear local session state and require a fresh sign-in. Personal API keys are independent credentials and are not revoked by password recovery; disable or rotate them separately if an API key may be compromised.

Default anti-abuse settings:

| Setting | Default |
|---|---:|
| Code TTL | 10 minutes |
| Resend cooldown | 1 minute |
| Per email per hour | 3 |
| Per source IP per hour | 20 |
| Global per hour | 200 |
| Maximum verification attempts | 5 |

Inject the SMTP credential through `AXONHUB_SMTP_PASSWORD`. Never commit it to Git, Compose, or documentation. Configuration lives in `conf/conf.go` and `config.example.yml`; the sender is in `internal/server/mail/`.

Troubleshooting order:

1. For signup, confirm that the address belongs to an allowed domain. For recovery, confirm privately that it is the complete email of an existing activated account. Both paths normalize it to lowercase.
2. `400` means malformed input, an invalid email, or an invalid new password. The recovery submission endpoint also provides stable `invalid_email`, `invalid_password`, or `invalid_verification` codes. `invalid_verification` deliberately covers a mismatched, expired, consumed, or exhausted code/challenge without exposing account state.
3. `429` means the resend cooldown or an email, source, or global hourly limit was reached. Wait for the response/UI cooldown rather than attempting to bypass it through another purpose.
4. `503` means the verification service or asynchronous delivery queue was unavailable before acceptance. Check the system secret, executor capacity, and service logs first; never interpret it as “account not found.”
5. If recovery returned `202` but no message arrived, verify only the account's activated state, SMTP host/port/sender, presence of `AXONHUB_SMTP_PASSWORD`, and sanitized logs. Do not print the credential or plaintext code, and do not confirm account existence to the requester. An asynchronous SMTP failure invalidates that undelivered challenge; request another after the cooldown.
6. Successful delivery is not successful signup or reset. Check purpose, expiry, one-time consumption, attempt count, and, for signup, project initialization next.

## 5. Member journey

The Shared Resources page is the member home page and must keep all of the following visible:

1. How to use the service: compatible Base URL, API-key creation, and copyable legal model IDs.
2. Personal allowance: today's and this week's effective tokens, reset times, and permanent wallet balance.
3. Available models: model ID, vision, tools, reasoning, context, and maximum output.
4. Shared channels: name, provider, source, contributor, description, expiry, a complete expandable model list, measurable provider quota or an explicit unavailable state, project-wide cumulative effective tokens, and detailed health.
5. How to contribute: a prominent Donate Channel action and a short explanation of rewards.
6. Transparent statistics: user ranking, model ranking, and contributor attribution.

A member calls the gateway with a personal API key. They are not adding an upstream provider to their client. UI text must preserve this distinction.

## 6. Channel donation, model synchronization, and capabilities

### 6.1 Channel lifecycle

The contributor sets an expiry and can use native AxonHub provider authentication, including Coding Plans that are not centered on a static API key. Custom URLs remain supported; the server still enforces URL safety and credential protection.

Members see the public channel projection, while only the contributor and Owner can read or change general configuration. Only the contributor may change the expiry, the expiry cannot be cleared, and only the contributor may delete an active donation. A channel that exits or expires leaves production candidates but does not erase historical statistics or wallet rewards.

### 6.2 Automatic model catalog

Creating or updating a channel synchronizes its model list and maintains same-name associations and new catalog entries. The Owner should not create every model manually.

A channel model list has two explicit maintenance modes:

1. **Automatic synchronization** dynamically merges provider-discovered models with contributor-added models. Later synchronization may add newly discovered provider models.
2. **Authoritative custom list** makes the currently saved list the complete set that the channel publishes and routes. When a contributor removes a provider model, clears the list, or keeps only a subset of discovered models, the UI must disable automatic synchronization and save that list. Reopening or saving the editor must not select everything, repopulate removed entries, or rebound to the provider catalog.

The contributor can re-enable automatic synchronization; the next synchronization resumes the dynamic provider-catalog and manual-model merge. Mode selection and list maintenance belong to the contributor and must not become Owner workload.

Resolution order:

1. Use authoritative metadata from a known model card.
2. Merge the contributor's override for that channel.
3. For an unknown model or custom endpoint, use permissive defaults:
   - vision: supported
   - tool calling: supported
   - reasoning: supported
   - context: 1,000,000 tokens
   - input modalities: text and image
   - output modality: text

These are gateway capability declarations, not proof that every upstream reaches each limit. Contributors should correct vision, tools, reasoning, context, and maximum output for their own channels on Shared Resources.

For reasoning models, `/v1/models` advertises `low`, `medium`, `high`, `xhigh`, and `max`. This helps clients expose and pass through the field; it does not make the gateway the final judge of every upstream tier.

Key implementation:

- Model-list synchronization: `internal/server/biz/channel_model_sync.go`
- Automatic catalog and defaults: `internal/server/biz/model_catalog.go`
- Contributor overrides: `internal/server/biz/campus_catalog.go`
- OpenAI model capability response: `internal/server/api/openai.go`

## 7. Quotas, effective tokens, and the donation wallet

### 7.1 Two quota layers

Calls are constrained in this order:

1. API-key/profile quota, independently configured by the user.
2. Global account daily and weekly allowances. The Owner sets one pair of values that applies immediately to every current and future account; there are no per-user exceptions.

Default account allowances:

| Window | Default limit | Reset |
|---|---:|---|
| Daily | 16,000,000 effective tokens | 00:00 Beijing time every day |
| Weekly | 64,000,000 effective tokens | Monday 00:00 Beijing time |

The Owner edits both global values on the Users page. They are read on each quota check, so a change applies on the next check. Setting a value to `0` prevents the normal allowance from accepting newly metered calls.

There is no fixed account-wide in-flight request cap. Per-channel capacity controls
remain independent safeguards for upstream providers and must not be confused with
a user concurrency quota.

### 7.2 Effective-token definition

Daily allowance, weekly allowance, rankings, and donation rewards use effective tokens after cache-read input is removed:

```text
non-cached input = max(prompt_tokens - cached_read_tokens, 0)
effective tokens = max(non-cached input + completion_tokens,
                       total_tokens - cached_read_tokens)
```

Only cache reads are removed; cache writes remain real input. `completion_tokens` already includes reasoning tokens and is not added twice. If a compatible provider omits cache-read details, the gateway cannot infer unknown caching and accounts conservatively. A defensive ceiling limits one request to 10,000,000 effective tokens.

Implementation: `internal/server/biz/effective_tokens.go`.

### 7.3 Permanent wallet

The wallet starts at the first cutover recorded by the new release and does not backfill history. After a successful non-test call, an eligible contributor receives:

```text
reward = floor(request effective tokens / 2)
```

No reward is created for:

- a manual channel probe;
- use of the contributor's own channel;
- an Owner-owned channel;
- usage before the wallet cutover;
- a request with zero effective tokens.

Calls debit permanent wallet balance first; only the uncovered portion enters the daily and weekly allowance. An API-key custom quota is still checked first, so the wallet never bypasses a limit the user placed on that key.

The wallet uses a materialized balance and an append-only ledger. The successful usage transaction is the only settlement point. Backup and restore must preserve:

- `user_token_wallets`
- `token_wallet_ledgers`
- linked usage, request, channel, and user identities

Implementation and restore validation: `internal/server/biz/token_wallet.go`, `internal/server/backup/wallet_ops.go`, `internal/server/backup/wallet_restore.go`.

### 7.4 Quota boundary

Quota checks use settled usage before a request starts and do not reserve the request's maximum usage. One large request or concurrent requests can settle past a limit; the next new request is rejected after that usage is recorded. Do not describe this as a strict in-request truncation.

## 8. Fair rotation, affinity, and failover

### 8.1 Selection

Production candidates must be enabled, unexpired, support the target model, and be currently healthy. Among them, the rotation score favors channels selected by fewer recent sessions and decays after inactivity. Affinity hits and manual probes do not advance fair-rotation counters. The goal is long-run best-effort fairness, not exact token equality.

A conversation prefers its previously successful channel to protect upstream caches and reduce risk controls:

- Explicit trace/session affinity wins when available.
- Without a session marker, the gateway uses the system/developer/first-user prefix and creates a project- and user-isolated HMAC digest.
- Raw prompt text is never retained in the affinity map. Input is capped at 8 KiB; the in-process map is capped at 4,096 entries with a sliding six-hour TTL.
- A channel is bound only after semantically valid output. Normal text, pure tool calls, reasoning, refusals, and media all count.

Affinity and fairness counters are process-local. Horizontal scaling needs shared state or an explicit acceptance of cross-instance misses.

### 8.2 Failures

- Empty responses, configured retryable HTTP statuses, and status-less TLS/connection/decode/timeout failures can be retried.
- Retry the same channel once, then fail over to a healthy channel.
- If a stream ends without a terminal event before its first semantic content is committed, treat it as incomplete and enter same-request failover.
- An explicit `model not supported` skips duplicate mappings to the same actual model within that channel before trying another model or channel. It does not declare that model unavailable on other accounts or channels.
- A `429` quota/rate-limit response moves directly to cross-channel selection instead of consuming repeated same-channel attempts. An upstream `402` puts that shared provider channel into a bounded five-minute cooldown before automatic recovery probing; the client receives a scoped `503 upstream_shared_quota_exhausted` rather than a misleading personal billing error, while the execution keeps its real upstream `402`.
- Unconfigured `401/403` failures are not repeated on the same channel. Cross-channel failover may still continue so one invalid or unauthorized shared credential does not terminate the user's request.
- When multiple attempts are exhausted, return HTTP `503` with code `upstream_candidates_exhausted`, category counts, the final upstream HTTP status, and sanitized final provider text. Keep each execution's real status; do not let the final `400` or `402` hide the preceding incomplete streams or authentication failures.
- For Responses streams, only a parseable `response.completed` event with a non-failed response snapshot is a successful terminal; a transport `[DONE]` marker is never enough. Preserve `response.incomplete` and its real reason, and convert `response.failed`, `response.cancelled`, and top-level errors into honest failures; token usage or a generic finish reason must not overwrite that terminal state.
- Failed pre-commit routes retain execution diagnostics but do not claim the logical request's single UsageLog settlement. The final delivered route owns quota, wallet, ranking, and channel attribution. Once semantic text or a tool call has actually been committed, an incomplete partial response remains honestly failed but its reported usage is settled because it was delivered and cannot be replayed.
- A stream already committed to the client cannot be replayed transparently. Mark the channel unhealthy without faking a seamless retry.
- A decoded response with no semantic output is a failure; a tool-only response is not empty.
- Transiently failing donated channels enter cooldown/circuit-breaking, leave the candidate set temporarily, and rejoin after recovery. They are not permanently auto-disabled.

The one-time `campus_sharing_policy_v1` migration selects round robin, one same-channel retry, and empty-response detection. It does not overwrite later Owner changes.

Production uses `1` same-channel retry, up to `10` cross-channel retries, and a `90`-second first-event timeout. The timeout only bounds a connection that produces no upstream event; it does not truncate a long response after normal output begins.

### 8.3 Codex Responses Lite compatibility

The Codex Responses Lite header and `reasoning.context: "all_turns"` form one invariant. For a Codex channel receiving a real Responses Lite request, the gateway preserves only the client's request envelope instead of rebuilding away Lite-only fields. Responses still pass through AxonHub's normal transformer and terminal validation. Model mapping, gateway-owned authentication and `Chatgpt-Account-Id`, channel overrides, prompt injection/protection, `store: false`, `stream: true`, and `reasoning.context: "all_turns"` take precedence. Ordinary Responses requests and non-Codex channels do not receive this scoped request compatibility. If `unsupported_value` still appears, inspect final outbound metadata and the client version before blaming the proxy egress.

Key implementation:

- Rotation score: `internal/server/orchestrator/lb_strategy_rr.go`
- Session affinity: `internal/server/orchestrator/session_affinity.go`
- Retry classification and exhausted-candidate summary: `internal/server/orchestrator/retry.go`, `llm/pipeline/upstream_error.go`
- Responses Lite invariant and scoped request-envelope compatibility: `llm/transformer/openai/codex/outbound.go`, `internal/server/orchestrator/override.go`, `internal/server/orchestrator/pass_through.go`
- Responses terminal semantics and health: `llm/transformer/openai/responses/`, `internal/server/orchestrator/performance.go`
- Donated-channel disable protection: `internal/server/orchestrator/channel_auto_disable.go`
- One-time migration: `internal/server/biz/system.go`

## 9. Health and manual probes

The Shared Resources UI supports five states:

- `healthy`: recent success without material failure;
- `degraded`: recent success and failure coexist;
- `unhealthy`: recent failures only or administratively disabled;
- `recovering`: the observation period after cooldown;
- `unknown`: insufficient production or probe evidence.

Every project member may probe a public channel. The server authorizes the member and uses the real channel configuration internally; the member never needs or receives its credential. A member may select one synchronized model to test exactly that model, or leave the selector on Auto for the safe fallback chain. The server rejects model IDs outside the channel's public synchronized list.

The model candidate order is adaptive:

1. the configured default test model, while still synchronized;
2. models recently successful in real API traffic;
3. remaining synchronized models, version-aware rather than array-position order.

Only explicit model-not-found, unsupported-model, or invalid-model failures such as `400`, `404`, and `422` try another model. Authentication, rate limiting, TLS, networking, and provider failures remain honest and are not hidden by model fallback.

The response includes actual model, total latency, HTTP status, attempt chain, and sanitized upstream error text. The UI scopes a green result to the actual tested model and never presents it as proof that every model on the channel works. Each user/channel pair has a 30-second click cooldown. Probes:

- do not consume daily or weekly allowance;
- do not enter rankings or fair-rotation accounting;
- do not create or debit wallet rewards;
- do update production health.

Implementation: `internal/server/api/campus_catalog.go`, `internal/server/biz/campus_catalog.go`.

## 10. Statistics, diagnostics, and privacy

### 10.1 Rankings

User and model rankings default to today and support week and month. The model list is capped at 50. The user list keeps the top 50 and appends the current user when they are outside the top 50. It prefers nicknames. The model ranking separates effective, input, cached-read, output tokens, and metered request count.

Only real metered API traffic is ranked. Manual probes are excluded. There is no monthly account quota, so the monthly ranking must not invent a monthly-limit percentage. Implementation: `internal/server/gql/campus_leaderboard.go`.

### 10.2 A user's own API activity

A user can inspect only the last six hours of activity produced by their own API keys and can filter by key. The server reads at most 200 requests and returns at most 100 events. The summary aggregates input, cached-read, output, and effective tokens plus success/error counts by key. Event rows contain time, key name and last four characters, model, status, HTTP status, total latency, and sanitized error. The current event DTO does not expose per-request token fields or time to first token.

It deliberately omits:

- prompts and response bodies;
- request and response headers;
- IP addresses and full URLs;
- full API keys;
- other users' activity.

Error summaries are capped at 600 characters and scrub credentials, cookies, sensitive query values, and email addresses. A scheduler removes error text older than six hours every 15 minutes. Non-body accounting metadata remains available for quotas and rankings.

Implementation: `internal/server/biz/campus_catalog.go`, `internal/server/biz/request.go`, `internal/server/scheduler/`.

### 10.3 Body storage is a production invariant

Upstream code defaults may still enable request and response body storage. Campus production relies on the database `storage_policy` override. Verify it after every deployment, restore, or new database:

```json
{
  "store_chunks": false,
  "store_request_body": false,
  "store_response_body": false
}
```

A sanitized activity panel does not prove that body storage is disabled. Never print other `systems` rows, bodies, API keys, SMTP credentials, or channel credentials to terminals, issues, or GitHub.

## 11. Channel transparency, model benchmarks, and friend links

### 11.1 Public channel cards

The provider-quota battery is visible to every signed-in project member and is projected from enabled, unexpired channels through a strict provider-specific allowlist. Members are read-only and cannot refresh, reset, or manage quota state; the Owner retains management actions. Provider quota describes an upstream subscription or Coding Plan and is separate from the campus 16M/64M effective-token allowance.

Each public channel card on Shared Resources must also:

- show the remaining percentage for the tightest provider-reported window and its known reset time when measurable data exists;
- honestly show unavailable, loading, or read failure when the provider cannot be queried or supplies no measurable field; missing data must never be interpreted as `0%` used or `100%` remaining;
- preview a small number of models and provide an explicit control that expands the complete model list; model IDs must wrap in full instead of being replaced by an ellipsis;
- show the channel's cumulative effective tokens within the current project. This follows section 7.2 and excludes manual probes; it is not the upstream account's lifetime consumption.

Implementation: `internal/server/biz/campus_catalog.go`, `internal/server/api/provider_quota_view.go`, `frontend/src/features/campus-resources/`, `frontend/src/features/system/data/quotas.ts`, and `frontend/src/components/quota-badges.tsx`.

### 11.2 Model performance and cost

The Model Performance and Cost page directly embeds the official Artificial Analysis `https://artificialanalysis.ai/embed/llm-leaderboard` iframe and keeps a link to its official model leaderboard. The same original leaderboard helps members compare capability and cost. If the frame fails, the page only presents a failure explanation and the direct official link.

Maintenance boundaries:

- do not scrape, cache, proxy, crop, redraw, or republish leaderboard data;
- do not read or rewrite the iframe DOM;
- the external leaderboard is selection guidance only and never participates in channel routing, health, campus allowances, wallet rewards, or billing;
- actual callable models remain defined by Shared Resources and `/v1/models`; absence from Artificial Analysis does not mean a channel is unavailable.

Implementation: `frontend/src/features/model-benchmarks/`, `frontend/src/routes/_authenticated/project/model-benchmarks/`.

### 11.3 Friend links

Friend links are Owner-managed in General Settings. Name and URL are required; description is optional. Only absolute credential-free `http://` or `https://` URLs are accepted. Names and URLs are unique, and the list is capped at 20. Members see a read-only sidebar group.

Implementation: `internal/server/biz/system.go`, `internal/server/gql/system.resolvers.go`, `frontend/src/features/system/components/general-settings.tsx`.

## 12. Campus extension endpoints

| Method | Path | Authorization and purpose |
|---|---|---|
| `POST` | `/admin/auth/signup/verification` | Public; request a UCAS email code |
| `POST` | `/admin/auth/signup` | Public; create a verified member |
| `GET` | `/admin/campus/resources` | Project member; models, channel models/health/cumulative effective tokens, account allowances, and wallet |
| `GET` | `/admin/campus/api-activity` | Project member; own six-hour activity only |
| `POST` | `/admin/campus/channels/:id/probe` | Project member; probe a public channel |
| `GET` | `/admin/campus/channel-model-capabilities` | Contributor; own channel capabilities |
| `PATCH` | `/admin/campus/channel-model-capabilities` | Contributor; update own override |
| `GET` | `/admin/provider-quotas` | Project member; read-only provider quota projection |
| `POST` | `/admin/graphql` | JWT; rankings, Owner settings, general admin |
| `GET` | `/v1/models` | API key; callable models and capability declarations |

Successful `/admin/campus/*` and `/admin/provider-quotas` responses should use `Cache-Control: private, no-store`.

## 13. Code map

| Area | Main entry points |
|---|---|
| Routes and middleware | `internal/server/routes.go` |
| Email signup | `internal/server/api/auth.go`, `internal/server/biz/auth.go` |
| User and UCAS domains | `internal/server/biz/user.go` |
| Resources, channel models/cumulative usage, health, activity, overrides | `internal/server/biz/campus_catalog.go` |
| Channel probe HTTP | `internal/server/api/campus_catalog.go` |
| Automatic model catalog | `internal/server/biz/model_catalog.go`, `channel_model_sync.go` |
| Effective tokens | `internal/server/biz/effective_tokens.go` |
| Daily and weekly allowance | `internal/server/biz/quota.go`, `user_daily_quota_settings.go` |
| Wallet settlement | `internal/server/biz/token_wallet.go` |
| Fair rotation and affinity | `internal/server/orchestrator/lb_strategy_rr.go`, `session_affinity.go` |
| Retry and health | `internal/server/orchestrator/retry.go`, `performance.go` |
| User/model rankings | `internal/server/gql/campus_leaderboard.go` |
| Provider quota projection | `internal/server/api/provider_quota_view.go` |
| Shared Resources and channel quota UI | `frontend/src/features/campus-resources/`, `frontend/src/features/system/data/quotas.ts` |
| Channel model-selection UI | `frontend/src/features/channels/components/channels-action-dialog.tsx`, `frontend/src/features/channels/utils/model-selection.ts` |
| Model performance and cost UI | `frontend/src/features/model-benchmarks/`, `frontend/src/routes/_authenticated/project/model-benchmarks/` |
| API activity UI | `frontend/src/features/apikeys/` |
| Owner global allowance UI | `frontend/src/features/users/components/user-daily-quota-settings-card.tsx` |
| Owner friend links UI | `frontend/src/features/system/components/general-settings.tsx` |
| Wallet backup and restore | `internal/server/backup/wallet_ops.go`, `wallet_restore.go` |

## 14. Change and validation matrix

Compilation alone is not acceptance. At minimum, cover:

| Area | Minimum validation |
|---|---|
| Signup | Four legal domains, look-alike rejection, expiry/replay, fail-closed SMTP, no Owner creation |
| API keys | Duplicate names, ownership isolation, last-four distinction |
| Channels | Contributor CRUD, public projection, no Owner deletion while active, expiry, no credential leak |
| Model sync | Same-name association, new models, unknown defaults, contributor overrides, reasoning tiers; automatic provider/manual merge; remove, clear, and subset actions enter authoritative mode and survive reopen/save; re-enabling restores dynamic synchronization |
| Quotas | 16M/64M defaults, immediate global Owner update, Beijing boundaries, key quota first, overshoot boundary, and no account-concurrency rejection above four in-flight requests |
| Wallet | Floor 50%, self/Owner/probe exclusions, wallet first, concurrent settlement, backup/restore |
| Rotation | Healthy best-effort fairness, affinity, recovered re-entry, empty output, tool-only output, status-less errors |
| Probes | Adaptive model, honest status/error, model-only fallback, cooldown, no accounting |
| Privacy | Body/chunk storage disabled, sanitization, six-hour scrub, no cross-user activity |
| Shared Resources UI | Use/donate paths, no lost information, five health states, attribution, both rankings, quota battery; measurable/unavailable channel quota distinction, complete model expansion, correct cumulative effective tokens, and no desktop or mobile overflow |
| External model leaderboard UI | The original Artificial Analysis iframe loads, loading/failure fallbacks and the official direct link work, and the page does not affect routing, health, allowances, wallet, or billing |

Add tests for new semantics instead of changing only the visible UI.

## 15. Safe release

### 15.1 Before release

1. On the Mac, inspect `git status --short --branch`, the target commit, and untracked files.
2. Create a clean build directory with `git archive <commit>`; never build directly from a dirty working tree.
3. Build the frontend with Node 20 and pnpm 10, then embed the complete `frontend/dist` into Go.
4. Build with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` and inject version, full commit, and build time.
5. Verify image platform, entrypoint, contents, and SHA-256 before upload.
6. Never build on the production server and never run `docker prune`.

The repository root `docker-compose.yml` is an upstream PostgreSQL example, not the Tencent production Compose. Never run it as the production definition.

### 15.2 Production preflight

Read-only verify the actual SSH target, hostname, disk, Compose files, service, container, image, mounts, and network. Do not print complete environment variables, full `docker inspect`, raw credentials, or raw request logs.

Capture a baseline:

- AxonHub container ID, image, start time, restart count, and data volume;
- all other container IDs and start times;
- PID and activation time of protected HAProxy, sing-box, and FRP services;
- backups and hashes of both production Compose files.

### 15.3 Data snapshot

AxonHub performs automatic schema migration on startup. Before switching:

1. Stop AxonHub only; never use `compose down`.
2. Copy `axonhub.db` plus existing WAL and SHM files as-is.
3. Use the SQLite backup API to create a consistent snapshot with WAL merged.
4. Run `PRAGMA quick_check` and `PRAGMA foreign_key_check` on the snapshot.

The live database, WAL, and SHM are not stale backups. Never delete them as cleanup.

### 15.4 Switch

Change only the AxonHub image in the production override:

```bash
docker compose -f <base-compose> -f <production-override> config --images
docker compose -f <base-compose> -f <production-override> up -d --no-deps --force-recreate axonhub
```

Never restart the Docker daemon, host, proxy, relay, or unrelated application.

### 15.5 Release gates

All must pass:

- internal and public `/health` return 200 with the target build commit;
- root HTML returns 200 and every referenced application JS/CSS/favicon asset returns 200;
- unauthenticated `/v1/models` returns 401;
- unauthenticated `/admin/provider-quotas` returns 401;
- an authenticated member can read resources and provider quotas, while a non-member is denied;
- a member probe shows actual model, HTTP status, and sanitized error;
- SQLite `quick_check=ok` with zero foreign-key violations;
- all three body/chunk fields in `storage_policy` remain `false`;
- the data-volume source is unchanged and AxonHub restart count is healthy;
- every other container and protected HAProxy/sing-box/FRP baseline is unchanged.

`/health` does not inspect the database and cannot prove that the SPA assets are complete.

### 15.6 Sticky production egress

The server-local sing-box URLTest group uses `tolerance: 30000` ms, `interval: 61s`, and `interrupt_exist_connections: false`. It keeps the current outbound while its health probe succeeds and fails over only after that probe fails. A recovered or lower-latency node must not preempt a healthy current outbound.

The live proxy configuration and all credentials remain server-local and are never committed. After a proxy configuration reload, verify that the sing-box PID, restart count, and listeners are unchanged, and that protected FRP/HY2 services and AxonHub remain unaffected.

## 16. Rollback

1. Stop only the failed new AxonHub container.
2. Move the database files created by the new version into an isolation directory. Preserve them; do not delete.
3. Restore the old Compose image reference.
4. If compatible, run the old image against the current database to preserve writes made after the switch.
5. Restore the pre-release consistent snapshot only for migration or schema incompatibility.
6. Recreate AxonHub only with `up -d --no-deps --force-recreate axonhub`.
7. Repeat HTTP, asset, database, privacy, and protected-service baseline checks.

Restoring a pre-release snapshot returns to pre-switch state and cannot honestly promise preservation of every write made while the new version was open. Keep the failed database for reconciliation.

## 17. Symptom guide

| Symptom | Check first | Do not start with |
|---|---|---|
| Blank UI | Root HTML, every referenced asset, browser console, embedded commit | Restarting proxies or host |
| `/v1/models` is 401 | Whether the gateway API key is absent; unauthenticated 401 is correct | Making it public |
| Owner probe green, member probe fails | Member probe path, candidate model, actual status and sanitized error | Replacing the error with “unavailable” |
| Intermittent channel failure | Five-state health, breaker, one same-channel retry, outbound proxy/TLS | Deleting the donation |
| Region error | Container proxy variables, Docker-reachable proxy address, actual egress | Changing inbound HAProxy |
| Unexpected user allowance | Effective tokens, wallet debit, Beijing windows, key quota | Per-user legacy-field exceptions |
| Wallet did not grow | Probe/self-use/Owner exclusion, cutover, successful effective-token settlement | Editing balance directly |
| Old error disappeared | Six-hour retention and 15-minute scrub | Enabling body storage |
| Provider quota is empty | Enabled/unexpired channel and provider allowlisted fields | Exposing raw quota JSON |

## 18. Maintenance commit checklist

For every functional change:

1. Record branch, HEAD, and dirty files first; preserve user-owned work.
2. Delete superseded dead code instead of keeping two paths.
3. Update Chinese and English docs, both UI locales, and tests together.
4. Never commit `.env`, credentials, channel secrets, databases, logs, build caches, or image archives.
5. Make one independently reversible commit per step, using `feat`, `fix`, `refactor`, `chore`, or `backup`.
6. Tag releases as `save-YYYY-MM-DD-description`.
7. After pushing, verify that remote branches and the tag point to the intended commit.

When business semantics change, this handbook is part of the change, not an after-release note.
