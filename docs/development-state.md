# Aevor — Development State

Current state of the platform repository.
NOTE: the canonical project docs live in the `docs` repo at `docs/development-state.md`. Keep this mirror in sync or delete it; do not let the two diverge.

## Branch / baseline

- Working branch: `feature/github-repositories` (stacked on `fix/users-upsert-token-column`, which is ahead of `main` @ `1040ebc`). Task 2a (`GET /repositories`) lives on the new branch; merge order: fix branch first, then feature. The old local+remote branches `feat` and `backup/wip-feat-668fa85` are STALE.

## GitHub repository discovery (Task 2a, 2026-08-23)

- NEW: authenticated `GET /repositories` — Handler (`internal/repositories/handler.go`) → Service (`service.go`) → existing `internal/github.Client.ListUserRepositories` (GET `/user/repos?sort=full_name&direction=asc&page=&per_page=`, Link-header pagination → `has_more`) → GitHub API.
- Identity comes ONLY from `RequireAuth` JWT; token retrieval/decryption via new `users.Service.DecryptedGitHubToken` (403 `github_token_missing` when absent; decrypt failure maps to 500 `internal`); GitHub rejecting the stored token maps to 401 `github_token_invalid` (re-login remedy); rate limit surfaces as 429 `github_rate_limited`; all other GitHub failures collapse to 500 `github_unavailable`.
- Response DTO is field-by-field mapped (id/name/full_name/description/private/default_branch/owner_login/html_url/clone_url/api_url + page/per_page/has_more); plaintext and encrypted tokens are asserted absent from success and error bodies by tests.
- No persistence added: repositories are fetched live from GitHub per request (discovery first). Pagination params validated (400 `invalid_pagination`), per_page clamped at 100.
- Verified: full suite green (`go build/vet/test/-race`), live server checks pass (health, unauth 401, missing-token 403, invalid-token 401 via real GitHub), fixture cleanup verified 0 rows.
- KNOWN ISSUE (deferred, pre-existing): GORM default logger in `pkg/database/postgres.go` logs SQL errors with bound values (incl. ciphertext) to stdout — tighten in a dedicated task.
- Deferred docs duty: add `GET /repositories` contract to canonical `docs/api-spec.md` once the `docs` repo branch situation (`feat/ai-docs`, untracked state files) is cleaned up.

## Security gate status (verified 2026-08-22)

- FACT: no ref tip tracks `.env`; `.gitignore` covers `.env`/`.env.*` (`check-ignore` confirms).
- FACT: the leaked commits `b9784cd` and `fd9752c` STILL EXIST and are ancestors of `origin/main` — **git-history purge is NOT done**; secrets remain recoverable from remote history until filter-repo + force-push (owner decision).
- UNVERIFIABLE locally: GitHub-side OAuth secret revocation/rotation — owner must confirm.

## Broken-functionality repairs (2026-08-22)

- **FACT:** the OAuth re-login upsert column mismatch was LIVE on `main` despite earlier reports of being fixed — no commit on any branch ever contained the correction (`git log --all -S "git_hub_access_token"` empty). `UpsertByGitHubID` referenced nonexistent column `github_access_token` while GORM maps `User.GitHubAccessToken` → `git_hub_access_token`; every repeat login failed with `column excluded.github_access_token does not exist`.
- FIXED: repository clause corrected + column mapping pinned in the model tag + schema-consistency regression tests (`repository_upsert_test.go`, proven to fail against the old code) + opt-in real-Postgres integration test (`repository_integration_test.go`, gated by `AEVOR_TEST_DATABASE_DSN`) covering insert + conflict-update paths. Integration test PASSES against live PostgreSQL 16 (localhost:5432).
- FIXED: JWT test time bomb — four tests issued tokens at fixed 2026-08-15 (7-day TTL) and parsed with wall-clock validation, failing permanently from 2026-08-22. `parsedClaims` now uses `jwt.WithoutClaimsValidation()`; validity coverage unchanged.
- Verified 2026-08-22 (post-review): `gofmt -l .` clean; `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...` all pass; integration test passes against live PostgreSQL 16 (localhost:5432) leaving schema and existing rows untouched; server boots, `/health` 200, `/auth/github/login` 302 with PKCE S256 params, `/users/me` uniform `401` without JWT; production auth code (`jwt.go`/`middleware.go`) untouched by the branch.
- Reviewed 2026-08-23 (independent re-review of the whole branch diff): one defect found and fixed — `TestUserModel_TokenColumnNeverSerialized` checked `TagSettings["JSON"]`, which never contains struct `json:` tags (GORM parses only the `gorm:` tag into TagSettings), so it could not fail. It now asserts `field.Tag.Get("json") == "-"`; mutation-tested to FAIL when `json:"-"` is removed. Post-fix: build/vet/test/`-race` green; live integration test green against PostgreSQL 5432 with zero fixture residue; `/health`, login redirect (PKCE S256 + state cookie), `invalid_state` callback mapping, and unauthenticated `/users/me` 401 all verified against a running server; startup AutoMigrate produced no schema change (`git_hub_access_token` column unchanged — no migration required). No secrets in server logs (query strings skipped by gin logger).

## Environment note

- FACT (2026-08-22): local development PostgreSQL 16.15 now runs natively via Homebrew on host port `5432` (db/user/password = `aevor`). The Docker Compose stack on host port `5433` is NOT currently running. `.env` points at `localhost:5432`.

## Auth & users implementation (Task 1)

GitHub OAuth 2.0 + PKCE login lives in `services/api`:

- `cmd/server/main.go` — wiring: `auth.NewService(oauthConfig, userService, jwtManager, ghClient, cfg.GitHubTokenEncryptionKey)`.
- `internal/auth` — login handler (`/auth/github/login`), callback handler (`/auth/github/callback`), JWT manager (HS256 via golang-jwt v5), OAuth/PKCE helpers, `RequireAuth` middleware, `/users/me`.
- `internal/github` — GitHub API client (`GetCurrentUser`) with injectable base URL/HTTP client and a typed error taxonomy.
- `internal/users` — user model (UUID PK, unique `github_id`, `GitHubAccessToken *string` `json:"-"`), service, GORM repository (interface + `gormRepository`), AES-256-GCM `Encrypt`/`Decrypt`, user DTO.
- `pkg/config` — env config incl. `JWT_SECRET` and `GITHUB_TOKEN_ENCRYPTION_KEY` (32-byte hex, validated fail-fast).

### Part 7 — JWT issuance (complete, verified 2026-08-15)

- `HandleCallback` now signs a JWT (`jwtManager.Issue`, HS256, `sub` = Aevor user UUID, `exp` 7d) after the upsert and the callback returns `{token, user}` per design D7 (JSON body only — no cookie; browser transport strategy remains a frontend-task decision).
- Claims: `sub`/`iss`("aevor")/`aud`("aevor-api")/`iat`/`exp`(7d). No `jti` (design D5 does not require one). No GitHub token, OAuth state, PKCE verifier, or profile data in the JWT.
- `JWTManager` gained a defensive ≥32-byte secret guard (`ErrInvalidJWTSecret`) and an injectable clock (`WithClock`) for deterministic tests. `pkg/config` fails fast on a missing/short `JWT_SECRET`.
- Tests: `internal/auth/jwt_test.go` (full JWT matrix incl. claims, HS256 header, wrong-key/expired rejection, no sensitive claims, independent timing), `pkg/config/config_test.go` (missing/short secret fails safely), callback integration tests (success issues a verifiable JWT; failed auth / failed persistence / signing failure emit no token; JWT payload carries only Aevor identity).

### Part 8 — JWT authentication middleware (complete, verified 2026-08-15)

- `RequireAuth(manager)` enforces `Authorization: Bearer <Aevor JWT>` (approved transport; no cookie; identity never taken from query/body/custom headers).
- Flow: `bearerToken` (exact `Bearer ` prefix, non-empty) -> `manager.Verify` (HS256 whitelist via `WithValidMethods`, `WithIssuer`, `WithAudience`, `WithExpirationRequired`, `sub` parsed as UUID) -> verified `uuid.UUID` stored in Gin context under the typed `UserIDKey` -> `c.Next()`. Every failure aborts with the uniform `401 {"error":"unauthorized"}` via `abortUnauthorized` — never silently continues.
- New `GetAuthenticatedUserID(c)` helper returns `(uuid.Nil, false)` safely when no verified identity exists; handlers consume identity only from context (`GetMe` updated to use it) — no handler parses JWTs.
- JWT is never logged and never echoed in error bodies.
- Tests: `internal/auth/middleware_test.go` (12 test functions) — valid token succeeds; missing/malformed/tampered/expired/wrong-signature/missing-sub/invalid-sub/wrong-issuer/wrong-audience rejected; HS384/HS512 (same secret), `alg=none`, and explicit algorithm-confusion (valid RS256/ES256 signatures) rejected, with an HS256 control proving the whitelist is the discriminator; verified UUID reaches context + helper retrieves it; unauthenticated requests never reach the handler; JWT absent from logs/error responses; query param + `X-User-Id` header cannot override JWT identity; helper fails safely without middleware. Deterministic `tamperToken` helper (`-count=20` stable).

### Test matrix (all passing, including `-race`)

- `internal/auth/callback_test.go` — state/verifier/cookie handling, error mapping, single exchange, no token leakage, re-login replaces token while keeping the Aevor UUID, encryption failure persists nothing, DB failure leaks nothing, JWT issuance + no-token-on-failure paths.
- `internal/auth/jwt_test.go` — JWT claim/lifetime/algorithm/verification matrix.
- `internal/auth/middleware_test.go` — middleware extraction/validation/context/error-behavior matrix (Part 8).
- `internal/users` — `crypto_test.go` (AES-GCM round-trip, tamper/truncation/wrong-key/invalid-key-length rejection, unique ciphertexts), `service_test.go` (validation), `dto_test.go` (mapping + JSON excludes token), `find_or_create_test.go` (upsert behavior with a fake repository, encryption key never persisted).
- `pkg/config/config_test.go` — fail-fast validation of a missing/short JWT secret.

### Known gaps / not yet done

- Manual end-to-end OAuth RE-LOGIN verification in a real browser (the conflict/update persistence path is proven at DB level; a live second GitHub login still to be exercised by the developer).
- Token rotation / key-rotation handling is future work.

## Infrastructure

- `deployments/` — docker-compose stack (Postgres exposed on host port 5433 as `aevor-postgres`).
- `services/api` — Go module; `go build`, `go vet`, and `go test -race ./...` are clean.
