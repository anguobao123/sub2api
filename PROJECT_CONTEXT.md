# Project Context

- Updated: 2026-09-11 Asia/Hong_Kong.
- Branch: `codex/fusion-billing-20260911`; original main checkout contains unrelated changes and is protected.
- Production code: `efb3659fcb4e721c70ed5db29d195acdfbab7072`, built on actual 0.2.4 (`5de5e2bed035d43591a2e10e51f420ef6a84eb98`) with personal task billing (`158a5d097`). Deployed 2026-09-11 with billing enabled and Gateway group 2.
- Production inspection found Docker image tag 0.2.3 but `/app/sub2api -version` reports 0.2.4. Preserve the actual binary baseline rather than replacing it with the older image's program.

## Confirmed product requirements

- Native USD billing, adjustable initial USD 50 for the first mapping, stopping new paid requests when funds/plans are exhausted, and reusing native subscriptions.
- One dedicated native user and API Key per Suite user. Password login remains disabled; `concurrency=0` preserves native unlimited concurrency.
- No new payment or plan system; no currency conversion; no fabricated production subscription groups.
- Production currently uses active OpenAI standard group 2 for existing keys and has no subscription groups. This was inspected read-only.

## Implementation

- The internal `/internal/openclaw/v1` API is guarded by an independent bearer; platform identities are mapped using a server-keyed HMAC. Credentials stay in private configuration.
- First mapping grants the configured amount once; overview only looks up an existing mapping. Admin APIs adjust available balance, modify the default grant, and assign existing native OpenAI subscriptions.
- Managed task sessions use migration `238_openclaw_task_sessions.sql`; the original port uses `221_openclaw_usd_billing_bridge.sql`. Upstream 0.2.4 uses `237_add_minimax_platform.sql`.
- Per-task funds initially reserve `min(USD 1, available balance)`, without a fixed execution concurrency cap. This is an initial hold, not a task spending cap.
- HTTP Responses/compact requires the matching personal key, task lease, user and allowed model. Other paid transports are rejected for managed keys.
- Native price calculation and native request/key dedup lead to managed wallet capture within the same PostgreSQL transaction. Native wallet debit is replaced; subscription costs remain in their native branch.
- Insufficient balance preserves a known unsettled amount and blocks new paid requests. Administrative top-up/set retries known wallet debts once. Unknown pricing/usage is not assumed zero.
- Close and expiry preserve in-flight funds, allow late settlement and only then release the unused remainder. Close-before-open records persistent intent; closing/closed/unsettled renew is an idempotent no-op.
- Exact transport amounts use `{currency:"USD",minorAmount:"integer string",scale:8}`. Transactions expose nullable `chargedUsd`, `actualCostUsd`, `model`, `fundingSource` and token counts; cached inputs use `cachedInputTokens`. Subscription periods use `dailyUsedUsd`, `weeklyUsedUsd`, `monthlyUsedUsd`.
- Full internal contract: [OPENCLAW_BILLING_BRIDGE.md](docs/OPENCLAW_BILLING_BRIDGE.md).

## Verification and handoff

- Before the 0.2.4 merge, focused `TestOpenClaw` suites passed across config, service, repository, handler, middleware, routes and migrations. Server/Wire compilation passed.
- Six real PostgreSQL scenarios passed: exact debit/native dedup/refund, four parallel task holds, insufficient funds without borrowing another hold, top-up settlement/idempotency, expiry with late billing and close-before-open/cross-user protection, native subscription quota without wallet debit.
- The disposable PostgreSQL test database originally applied the old temporary migration filename 237. Subsequent merged-code tests must use a fresh disposable database rather than replay renamed DDL into it.
- Merged-baseline Wire, focused tests and fresh PostgreSQL migration/scenarios passed. Darwin ARM64 and Linux AMD64 binaries compiled; the production binary embeds the original 0.2.4 frontend.
- Full local Gateway acceptance used the actual CLI, a synthetic Responses upstream, native pricing and PostgreSQL: charged exactly USD 0.00020000 once, correct user/key/task ownership, frozen funds released, and zero balance rejected before another upstream call. Suite-to-Go real HTTP integration also passed.
- Production acceptance used one new synthetic Suite account with USD 50: real GPT-5.5 settled USD 0.015023, remaining balance USD 49.984977 and frozen USD 0. Setting this test account to zero then rejected a new model task with zero tokens and no additional fee records. No existing account was recharged or reassigned.
- Suite `5810b00` is active. Its initial node rollout exposed a missing enrolled capability; the administrator-only capability update now preserves node ID/key/adapter scope and rejects updates during active execution leases. The existing queued test ran once after this fix. Its original timed-out reply remains unknown and was not resent; portal and denial checks use synthetic callbacks, not real WeChat sends.
- Deployment release `20260911-efb3659fc` stores the captured actual old binary as a rollback image, private prior configuration and one required PostgreSQL dump under `/opt/sub2api/releases/personal-billing/`. Database restoration is not part of routine rollback. Existing environment, volumes and public routing were preserved; private bridge routing requires the Lexi source and independent bearer.
- Test containers/Redis/PostgreSQL were removed after acceptance. Scripts/reports and final binaries remain outside the repository. Future changes must preserve the deployed accounting data and the original source checkout's unrelated changes.
