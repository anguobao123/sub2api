# Sub2API OpenClaw Billing Bridge

## Start Here

- Read this file and `PROJECT_CONTEXT.md`, then inspect Git status and affected code before changes.
- This worktree is dedicated to `codex/fusion-billing-20260911`, based on the running Sub2API 0.2.3 revision `8fa67d477d6651a744754392a8982ea589c26ae6`. Keep the original checkout and other worktrees unchanged.
- The Fusion primary task coordinates Suite integration, credentials and deployment under the user's existing authorization; delegated porting and tests must stay within their assigned files and temporary environments.
- Keep the upstream LGPL-3.0-or-later license and copyright notices intact. New code must be original, narrowly scoped, and compatible with the existing license.

## Project

- Sub2API is a Go HTTP service with PostgreSQL persistence, Ent models, Redis support, and a Vue frontend.
- This branch adds an optional OpenClaw USD billing bridge without changing existing browser, admin, gateway, payment, or API-key contracts.
- The bridge uses the existing `users.balance` and `users.frozen_balance` columns transactionally, but keeps OpenClaw mappings, leases, grants, and usage events in separate tables.

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
- Backend build: `cd backend && CGO_ENABLED=0 go build -o bin/server ./cmd/server`; release flags are defined by the current `backend/Makefile`.
- Regenerate DI after provider changes: `cd backend/cmd/server && go generate`.

## Engineering Rules

- Go code follows the existing style: Go ESM-equivalent module layout, tabs as `gofmt` emits, explicit errors, and PostgreSQL `NUMERIC(20,8)` amounts.
- Use a separate `openclaw_billing_*` namespace. Never route bridge traffic through browser JWT, admin credentials, OpenAI gateway API keys, or the normal `usage_billing_dedup` table.
- Internal bridge requests require a separately configured bearer credential. The feature is disabled and fail-closed unless explicitly enabled and configured.
- Do not persist raw `platformUserId`, raw credentials, or raw chat/task content. Persist a server-keyed HMAC locator and opaque internal IDs only.
- OpenClaw usage events are append-only. Do not add them to generic usage cleanup paths or expose deletion endpoints.
- A usage event without both `pricingVersion` and `priceSnapshot` remains `pending_pricing` and must never charge a balance or capture a lease.

## Verification

- Bridge configuration, auth, mappings, grants, reservations, captures, releases, expiry, and usage ingestion need focused positive and negative tests.
- Inspect `git diff --check` and `git diff` before handoff.
- Push, deployment, production migrations and real credentials are coordinated by the Fusion primary task after the relevant changes are reviewed and tested. Temporary test containers must not expose ports or attach to production databases.

## Handoff

- Update `PROJECT_CONTEXT.md` after meaningful implementation and immediately before final handoff.
