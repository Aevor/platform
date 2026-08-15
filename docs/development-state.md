# Aevor — Development State

Current state of the platform repository.
NOTE: the canonical project docs live in the `docs` repo at `docs/development-state.md`. Keep this mirror in sync or delete it; do not let the two diverge.

## Branch / baseline

- Working branch: `feat`.
- Baseline: `6e18c0f` — merged Task 2 implementation plus follow-up PRs #19 (AES-GCM crypto tests), #20 (users service validation tests), #21 (user DTO tests), #22 (health `StatusOK` constant), #23 (github request build error wrapping).
- Task 1 Parts 1–8 changes are UNCOMMITTED in the working tree. Nothing committed/pushed.

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

- Callback error surface still exposes client codes (`github_api_unauthorized`, `github_rate_limited`, `github_invalid_response`, `github_api_error`) beyond design §6; reconcile during Task 1 close-out.
- Real-DB integration tests for the upsert path require a running test Postgres (currently covered by fake-repo unit tests).
- End-to-end `/users/me` verification against the issued JWT still to be exercised manually once credentials are confirmed.
- Token rotation / key-rotation handling is future work.

## Infrastructure

- `deployments/` — docker-compose stack (Postgres exposed on host port 5433 as `aevor-postgres`).
- `services/api` — Go module; `go build`, `go vet`, and `go test -race ./...` are clean.
