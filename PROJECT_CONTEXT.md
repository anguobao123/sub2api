# Project Context

- Updated: 2026-09-11 Asia/Hong_Kong.
- Branch: `codex/fusion-billing-20260911`; original main checkout contains unrelated changes and is protected.
- Current local integration: `68bb6d366` merges actual production 0.2.4 (`5de5e2bed035d43591a2e10e51f420ef6a84eb98`) with personal task billing (`158a5d097`). No production deployment of this feature yet.
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
- The opt-in HTTP fixture mounts only private bridge routes; it cannot prove the model Gateway chain. Full Gateway/CLI integration is being performed separately with synthetic upstream Responses and native PostgreSQL accounting.
- Suite has implemented node admission, personal wallet/admin APIs and pages. Its 398 local tests, build, and Edge tests passed; these do not yet imply production charging is enabled.
- Next: finish merged-baseline compilation and complete Gateway/Suite HTTP integration, then the Fusion primary task coordinates production deployment and isolated real-model acceptance under existing authorization.
