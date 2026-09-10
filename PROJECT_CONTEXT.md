# Project Context

- Last updated: `2026-09-11 Asia/Hong_Kong`
- Maintainer: `Codex`
- Status: `ported to current production baseline; focused tests and server compilation passed`
- Git baseline: `codex/fusion-billing-20260911` at `8fa67d477d6651a744754392a8982ea589c26ae6` (Sub2API 0.2.3).
- Reused changes: `ffa3e9ec` plus `c21144f`; the latter supplies decimal amounts and USD scale-8 transport. Config and Wire conflicts were resolved while preserving 0.2.3 services.
- The user's original administrator requirements confirm native USD billing, adjustable initial USD 50, exhaustion stopping, and reuse of Sub2API plans. The exact historical strict-mode question is not fully retained; do not equate an invented route name with the user's requirement.
- Current gaps: real priced-usage to capture linkage, Suite admission/results, administrator balance and plan integration, and real PostgreSQL acceptance. The installed service is unchanged; the bridge remains disabled.
- Current verification: official Go 1.27 container ran 31 top-level TestOpenClaw tests across config/service/repository/routes/middleware/migrations; handler compilation passed. CGO-disabled cmd/server compilation and its modified cleanup test passed, including wire_gen.go. The Linux ARM64 binary is compilation evidence only; no production service or database was changed. Temporary build containers were removed.
- Planned integration: give each billing user a dedicated Gateway key so native user pricing and subscriptions have the right owner. Capture real quantified cost in the existing usage_billing_repo transaction after native request/key deduplication, replacing wallet deduction for a funded request rather than charging twice. Keep subscription billing mutually exclusive. Pending requests must prevent premature lease release. This integration is not yet implemented.

## Goal

Add an optional, PostgreSQL-backed OpenClaw USD Billing Bridge to Sub2API. It must reuse the existing user balance and frozen-balance accounting safely while preserving all established Sub2API browser, admin, API-key, gateway, payment, and usage behavior.

## Current Scope

- Separate internal bearer-protected API at `/internal/openclaw/v1`.
- Stable opaque mapping from a server-keyed HMAC of `platformUserId` to an internal billing user/account.
- Idempotent configurable first-mapping USD grant, strict task budget lease reserve/capture/release/expiry, and immutable OpenClaw usage-event persistence.
- Batch usage ingestion at `POST /internal/openclaw/v1/usage-events:batch`.
- Events lacking both `pricingVersion` and `priceSnapshot` persist as `pending_pricing` without any balance deduction.

## Explicit Non-Goals

- No real payment integration, plan/subscription activation, real credential, deployment, remote write, Suite code change, or source-worktree modification.
- No generic usage cleanup or delete path for OpenClaw accounting events.

## Key Decisions

| Date | Decision | Reason |
| --- | --- | --- |
| 2026-08-13 | Use separate OpenClaw bridge tables and ID domain while transacting against `users.balance` / `users.frozen_balance`. | Reuses mature Sub2API balance storage without colliding with gateway billing deduplication or API-key semantics. |
| 2026-08-13 | Keep the bridge disabled and fail-closed unless its independent bearer, identity HMAC key, and feature flag are configured. | Prevents browser JWT/API-key reuse and accidental exposure of an internal interface. |
| 2026-08-13 | Treat incomplete price metadata as persisted `pending_pricing`, never as a charge. | Enforces the Suite contract and protects users from unpriced deductions. |

## Implementation Status

- Optional `openclaw_billing` config is default-disabled and fail-closed. Enabling it requires an independent bearer and an identity HMAC key, each at least 32 bytes; no real secret is stored here.
- `POST /internal/openclaw/v1/accounts:resolve` HMAC-locates a stable external platform identity without storing or returning its raw value. The first successful mapping creates a disabled synthetic internal user/account and one unique `initial_mapping` grant using the configurable default USD 50.0 balance credit.
- Task-budget leases reserve strictly from `users.balance` into `users.frozen_balance`, then capture or release explicitly. Stable idempotency keys, request fingerprints, PostgreSQL advisory transaction locks, active-task uniqueness, and lease expiry protect retries and concurrent calls.
- The lease expiry worker starts with the service, scans at startup and then at `min(30 seconds, TTL / 2)`, and is stopped through the application cleanup path.
- `POST /internal/openclaw/v1/usage-events:batch` persists real token counts, model, timing, task/session/node/Codex-thread identifiers, price evidence, and an immutable event fingerprint. Events missing either pricing version or price snapshot are `pending_pricing`; they never capture a lease or deduct balance.
- The bridge deliberately does not alter existing Sub2API browser, admin, gateway, payment, or generic usage-cleanup routes. The OpenClaw usage table is append-only and has `UPDATE`, `DELETE`, and `TRUNCATE` guards.
- The OpenClaw-only HTTP amount contract is fixed to `{ "currency": "USD", "minorAmount": "<decimal integer>", "scale": 8 }`. JSON numbers are rejected for request amounts and never emitted in responses. Bridge service/repository monetary values use exact `decimal.Decimal` values with PostgreSQL `NUMERIC(20,8)` scale; price snapshots also reject JSON numbers. The HTTP contract is implemented in `backend/internal/handler/openclaw_billing_handler.go`.

## Historical Verification (old baseline only)

| Check | Result | Notes |
| --- | --- | --- |
| Git branch and initial status | passed | Dedicated `codex/openclaw-usd-billing-bridge`, baseline `48eb3766d`. |
| Migration checks | passed | `go test -count=1 ./migrations`. |
| Bridge config checks | passed | `go test -count=1 ./internal/config`. |
| Bridge service checks | passed | `go test -count=1 -run 'TestOpenClaw' ./internal/service`. |
| Bridge repository checks | passed | `go test -count=1 -run 'TestOpenClaw' ./internal/repository`, including one-time grant transaction and strict balance guard. |
| Bridge auth and routes | passed | `go test -count=1 ./internal/server/middleware` and `go test -count=1 -run 'TestOpenClaw' ./internal/server/routes`. |
| Handler compilation | passed | `go test -count=1 -run '^$' ./internal/handler`; full server build also passed. |
| Server DI and build | passed | `go test -count=1 ./cmd/server`; `go build -o /tmp/sub2api-openclaw-billing-bridge-server ./cmd/server`. |
| Diff and format hygiene | passed | `git diff --check`; affected Go files are `gofmt` clean. |

## Handoff Notes

- Preserve the upstream LGPL-3.0-or-later `LICENSE` and existing attribution.
- Do not write raw `platformUserId` into SQL rows, logs, errors, or HTTP responses.
- No real PostgreSQL migration has been applied and no real Suite, payment, deployment, credential, multi-host, or browser integration evidence exists.
- A temporary local Go 1.26.5 toolchain was used for `gofmt`, focused bridge packages, and a server build. No external service has been invoked; only public Go toolchain/module downloads were used in `/private/tmp`.
- The bridge has no internal price table or price resolver. Suite supplies `pricingVersion`, `priceSnapshot`, and optional `estimatedUsd`; incomplete pricing is preserved as immutable pending evidence.
- Package/subscription activation and real payment are intentionally not enabled. Usage ingestion is evidence only; capture is an explicit caller-provided lease operation and is not inferred from usage automatically.
