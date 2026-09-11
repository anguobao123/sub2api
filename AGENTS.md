# Sub2API Fusion Upstream

## Start Here

- Read this file and `PROJECT_CONTEXT.md`, then inspect Git status and affected code before changes.
- This worktree is dedicated to `codex/fusion-billing-20260911`, based on actual Sub2API 0.2.4 revision `5de5e2bed035d43591a2e10e51f420ef6a84eb98`. Inspect the actual binary version: the container image tag may predate an in-app update. Keep the original checkout and other worktrees unchanged.
- The Fusion primary task coordinates Suite integration, credentials and deployment under the user's existing authorization; delegated porting and tests must stay within their assigned files and temporary environments.
- The default destination for future pushes is the `anguobao123` fork.
- Keep the upstream LGPL-3.0-or-later license and copyright notices intact. New code must be original, narrowly scoped, and compatible with the existing license.

## Project

- Sub2API is a Go HTTP service with PostgreSQL persistence, Ent models, Redis support, and a Vue frontend.
- Sub2API supplies replaceable upstream model execution and platform cost records. Suite on Lexi owns each customer's balance, prices, plans, admission, holds and charges.
- Two ordinary aggregate accounts separate user-task traffic (`fusion-user-workloads`) from platform-internal traffic (`fusion-platform-services`). Do not create a native account or key for each Suite user or reuse the administrator's key.
- `GET /v1/usage/requests` exposes only the authenticated ordinary API key's native request usage and exact decimal cost. It remains usable when the old bridge is disabled.
- The legacy OpenClaw wallet bridge is retired in production. Its code, migrations, account balances, mappings and history remain preserved; do not re-enable it as Suite's customer wallet.

## Important Paths

- `backend/internal/config/`: configuration defaults and validation.
- `backend/internal/service/`: domain contracts and application services.
- `backend/internal/repository/`: PostgreSQL repository implementations and SQL-focused tests.
- `backend/internal/handler/`: HTTP request/response handlers.
- `backend/internal/server/`: route registration and middleware composition.
- `backend/migrations/`: forward-only embedded PostgreSQL migrations.
- `backend/cmd/server/wire.go` and `wire_gen.go`: dependency injection source and generated output.

## Commands

- Backend format: `gofmt -w <changed Go files>`.
- Focused tests: `cd backend && go test ./internal/service ./internal/repository ./internal/handler ./internal/server/routes`.
- Tagged unit tests: `cd backend && go test -tags=unit ./internal/server`.
- Request usage checks: `cd backend && go test -tags=unit ./internal/repository ./internal/handler ./internal/server/middleware ./internal/server/routes -run TestAPIKeyRequestUsage -count=1`.
- Production build: `cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -trimpath -o bin/server ./cmd/server`; preserve frontend assets and embed the actual version/commit in release flags.
- Regenerate DI after provider changes: `cd backend/cmd/server && go generate`.

## Engineering Rules

- Go code follows the existing style, `gofmt` tabs and explicit errors. Native wallet amounts use PostgreSQL `NUMERIC(20,8)`; request cost lookup preserves the source decimal precision with `actual_cost::text`.
- Capture the native response's `X-Client-Request-ID`; request lookup adds the internal `client:` prefix and always filters by the authenticated API key ID. A missing row means `ready:false`, never a confirmed zero charge.
- Native usage input/cache counters are disjoint. Upstream costs are platform accounting information; Suite customer charges come from Suite prices and observed usage, not this cost field.
- Legacy internal bridge authentication remains separate from ordinary API keys, browser JWT and admin credentials. Keep `openclaw_billing.enabled=false` for the retired deployment.
- Do not persist raw `platformUserId`, raw credentials, or raw chat/task content. Persist a server-keyed HMAC locator and opaque internal IDs only.
- OpenClaw usage events are append-only. Do not add them to generic usage cleanup paths or expose deletion endpoints.
- Preserve retired unknown/unsettled records and legacy event semantics; do not infer zero cost, refund or replay from missing usage.

## Verification

- Request lookup changes need own-key, cross-key, missing-row, exact-decimal and disabled-key coverage. Changes to retained legacy bridge behavior still require its focused positive and negative tests.
- Inspect `git diff --check` and `git diff` before handoff.
- Push, deployment, production migrations and real credentials are coordinated by the Fusion primary task after the relevant changes are reviewed and tested. Temporary test containers must not expose ports or attach to production databases.

## Handoff

- Update `PROJECT_CONTEXT.md` after meaningful implementation and immediately before final handoff.
