# Project Context

- Updated: 2026-09-11 Asia/Hong_Kong.
- Branch: `codex/fusion-billing-20260911`; original main checkout and other worktrees remain protected. Future pushes default to the `anguobao123` fork.
- Running Sub2API: `88db4dbc62f3b78d43ec40a363bbc1fa98a44375`, actual version 0.2.4, with the existing frontend embedded. Running Suite: `194d939efb78ea8769e6495c9a4c54313120f516`, Schema9.
- The original production image tag had lagged behind an in-app update; preserve the actual 0.2.4 binary lineage rather than reverting to the older tagged program.

## Current accounting boundary

- Sub2API is a replaceable upstream provider. Suite on Lexi owns customer accounts, balances, price cards, plans, admission, holds and charges. Upstream cost never directly determines a customer debit.
- `fusion-user-workloads` is one ordinary native account for all user-task upstream traffic, with an initial USD50 platform cost budget and one shared private key. `fusion-platform-services` is a separate ordinary account with USD0, no key and no current model traffic. Both use `concurrency=0` and group 2; neither is an OpenClaw mapping or the administrator's account.
- Native users/keys/usage remain normal Sub2API platform workload records. Do not reintroduce a native account/key for every Suite user. Customer prices and balances now change in Suite independently of the upstream provider.
- `GET /v1/usage/requests?request_id=<X-Client-Request-ID>` authenticates an ordinary API key and queries only its native `client:<id>` record. It returns `ready`, model, disjoint input/output/cache counts, nullable service tier and exact decimal-string `actualCostUsd`; missing rows are not zero-cost evidence.
- This read endpoint works with the old bridge disabled and with an exhausted service wallet/key quota, while disabled users/keys remain rejected. It does not touch key last-used metadata. Details and historical contracts remain in [OPENCLAW_BILLING_BRIDGE.md](docs/OPENCLAW_BILLING_BRIDGE.md).

## Completed migration and retained history

- Fusion's primary integration task exported and imported 2 mapped accounts, 3 settled requests, 2 grants, 1 adjustment and 6 initial model prices. The two opening balances, USD0 and USD49.776191, were preserved; frozen funds, in-flight/unsettled requests and subscriptions were zero before cutover.
- All old mapped users and keys were disabled. Native administrator APIs invalidated the exact authentication caches; old keys returned 401/403. Existing notes identify the migration. No account, key, balance or historical billing record was deleted.
- Only `openclaw_billing.enabled` changed to false in the private source configuration. Identity HMAC, JWT, prior balances and history were retained. The new Sub2API image was restarted, and post-restart retirement verification passed.
- Legacy bridge code and migrations `221_openclaw_usd_billing_bridge.sql` / `238_openclaw_task_sessions.sql` remain for historical interpretation and controlled recovery. Their old per-user native wallet design is retired, not the active integration contract.
- The prior native-wallet release and its historical acceptance evidence remain rollback/reference material. Restoring an old program must not silently re-enable customer charging on the retired wallet or overwrite the migrated Suite ledger.

## Actual validation and handoff

- The new endpoint's focused repository, handler, middleware and route tests passed, including exact decimal output, own-key/cross-key lookup, pending rows, disabled credentials and zero-funds reads. Linux AMD64 production build passed with the original frontend and the exact Sub2API commit above.
- Production acceptance recorded by the Fusion primary task used synthetic Bot delivery and the real Node/GPT-5.5 path: local charge and upstream cost were each USD0.014998, frozen funds returned to zero, and the aggregate key/native request-ID cost lookup matched.
- Setting that synthetic account's local balance to zero rejected its next task before model execution: zero tokens, no added upstream model requests or fees. Its original zero balance and chat settings were restored; no real WeChat send was used as evidence.
- The three Suite services were active and the portal returned HTTP200 after cutover. The 14 original Windows task records and unknown states were retained without replay.
- Earlier native-wallet PostgreSQL and real-CLI checks cover the retired implementation only; they are not claims that it remains active. Scripts, private export/import artifacts and release binaries stay outside the repository.
- Current implementation and cutover are integrated. Preserve the migrated financial history and the two workload accounts in follow-up work. Source commits and pushes are coordinated by the Fusion primary task.
