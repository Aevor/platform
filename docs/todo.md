# Aevor Development TODO

Tracking the progression of Aevor Task 1 (GitHub OAuth login + user registration).
Canonical project docs live in the `docs` repo (`docs/development-state.md`); this file mirrors the tracker for the platform repo.

## Completed

- **Task 1 Part 1 — OAuth login init:** `GET /auth/github/login` generates a random OAuth `state` + random PKCE `code_verifier`, stores both in a short-lived HttpOnly SameSite=Lax cookie, and redirects to GitHub with `code_challenge=S256`; `GenerateState`/`VerifyState` (crypto/rand + `subtle.ConstantTimeCompare`).
- **Task 1 Part 2 — OAuth callback:** `GET /auth/github/callback` verifies `state` (constant-time), clears the cookie, exchanges the single-use code with the `code_verifier` (exchange NEVER retried), maps errors (`invalid_state`, `github_authorization_denied`, `invalid_code`, `github_unavailable`).
- **Task 1 Part 3 — JWT + `/users/me`:** `JWTManager` (HS256 via golang-jwt v5, claims sub/iat/exp(7d)/iss/aud), `RequireAuth` middleware, `GET /users/me`; temporary public routes `POST /users`, `GET /users/:id`, `GET /users/github/:id` removed.
- **Task 1 Part 4 — GitHub profile client:** `internal/github` `GetCurrentUser`, injected HTTP client (10s default timeout), injectable base URL (`WithBaseURL`), User-Agent, Bearer auth, redirects disabled, typed error taxonomy (no token leakage).
- **Task 1 Part 5 — User upsert + encrypted token persistence wiring:** callback encrypts the exchanged token (`users.Encrypt`) then persists via `users.FindOrCreateByGitHubID` (`ON CONFLICT (github_id) DO UPDATE ... RETURNING`); Aevor UUID preserved on re-login, token replaced; `users.Repository` is an interface (concrete `gormRepository`) so service/callback logic is unit-testable with fakes.
- **Task 1 Part 6 — Secure GitHub Access Token Storage (verified 2026-08-15):** AES-256-GCM encrypt/decrypt (random nonce, `base64(nonce || ciphertext)`, tamper/wrong-key rejection), `GitHubAccessToken *string` `json:"-"` column, key from `GITHUB_TOKEN_ENCRYPTION_KEY` env (32-byte hex, fail-fast validation), re-login replaces token while preserving the Aevor UUID, token never in responses/logs/errors/DB-plaintext.
- **Task 1 Part 7 — Aevor JWT Issuance (complete, verified 2026-08-15):**
  - `HandleCallback` signs a JWT (`jwtManager.Issue`, HS256, sub = Aevor user UUID, exp 7d) after a successful upsert and the callback returns `{token, user}` per design D7.
  - Claims: `sub` (Aevor UUID), `iss` (`aevor`), `aud` (`aevor-api`), `iat`, `exp` (7 days). No `jti` (not required by the approved design). No GitHub token, PKCE verifier, OAuth state, or profile data in the JWT.
  - `JWTManager` gained a defensive ≥32-byte secret guard (`ErrInvalidJWTSecret`) and an injectable clock (`WithClock`) for deterministic tests; `pkg/config` already fails fast on a missing/short `JWT_SECRET`.
  - Browser transport = JSON response body only (D7). No cookie.
  - Tests: full JWT matrix (sub/iss/aud/iat/exp, HS256, verify with configured key, wrong key, expired, no sensitive claims, independent timing), config validation, and callback integration (success issues JWT; failed auth / failed persistence / signing failure emit no JWT; JWT payload carries only Aevor identity).
- **Task 1 Part 8 — JWT Authentication Middleware (complete, verified 2026-08-15):**
  - Transport (approved, unchanged from Part 7): `Authorization: Bearer <Aevor JWT>` header. No cookie, no query/body/header-derived identity.
  - `RequireAuth(manager)` refactored: `bearerToken(header)` extraction (exact `Bearer ` prefix + non-empty value) → `manager.Verify` (HS256 whitelist, exp/iss/aud, sub parsed as UUID) → verified `uuid.UUID` placed in Gin context under the typed `UserIDKey`; single uniform `abortUnauthorized` helper returns `401 {"error":"unauthorized"}`. Any failure aborts the request — never silently continues.
  - `GetAuthenticatedUserID(c)` helper added; fails safely (`(uuid.Nil, false)`) when no verified identity is present. Handlers consume identity from context — they never parse JWTs or accept client-supplied identity. `GetMe` now uses the helper.
  - JWT never logged; token never echoed in error responses; error response reveals only `{"error":"unauthorized"}`.
  - Tests (`internal/auth/middleware_test.go`): valid token succeeds; missing/malformed/tampered/expired/wrong-signature/missing-sub/invalid-sub/wrong-issuer/wrong-audience rejected; wrong HMAC alg (HS384/HS512 same secret), `alg=none`, and explicit algorithm-confusion (RS256/ES256 with valid keys) rejected; verified UUID reaches context + helper retrieves it; unauthenticated requests never reach the handler; JWT absent from logs and error bodies; query/`X-User-Id` header cannot override JWT identity; helper fails safely without middleware. Deterministic tamper helper (`tamperToken`) — `-count=20` stable.

- **Task 1 Part 9 — Authenticated `/users/me` endpoint test suite:** committed via PRs #33–#38 (see `docs/development-state.md` for the full list).

## Completed (2026-08-22 — broken-functionality repairs)

- **FIX — OAuth re-login upsert column mismatch (production bug):** `UpsertByGitHubID` referenced a nonexistent column `github_access_token` in the `ON CONFLICT DO UPDATE` clause, while GORM maps `User.GitHubAccessToken` to the column `git_hub_access_token`. First-time logins inserted fine, but every REPEAT login failed with `column excluded.github_access_token does not exist`. NOTE: this fix had previously been reported as done but never existed in any commit (`git log --all -S` confirms); it is now actually fixed.
  - `internal/users/repository.go`: corrected column to `git_hub_access_token`; conflict/assignment columns extracted into package-level vars (`upsertConflictColumns`, `upsertAssignmentColumns`).
  - `internal/users/model.go`: explicit `gorm:"column:git_hub_access_token"` tag pins the mapping (no physical schema change; AutoMigrate no-op).
  - New `internal/users/repository_upsert_test.go`: schema-consistency regression tests that validate every upsert-referenced column against the model's parsed GORM mapping (verified to FAIL against the old buggy code).
  - New `internal/users/repository_integration_test.go`: opt-in real-Postgres integration test (`AEVOR_TEST_DATABASE_DSN`) covering insert + conflict-update paths, UUID preservation, token rotation; skipped by default so unit runs stay hermetic. PASSES against live PostgreSQL 16 on localhost:5432.
- **FIX — time-bomb JWT tests:** `TestJWT_ContainsCorrectSubject/ContainsCorrectIssuedAt/ContainsCorrectExpiration/IndependentTokensHaveIndependentTiming` issue tokens at a fixed 2026-08-15 instant (7-day TTL) and parsed them with live wall-clock validation — permanently failing once 2026-08-22 arrived. `parsedClaims` now parses with `jwt.WithoutClaimsValidation()` (claim inspection only); validity semantics remain covered by `manager.Verify` and middleware tests. Suite green again.
- **REVIEW (2026-08-23) — token non-exposure test was ineffective:** `TestUserModel_TokenColumnNeverSerialized` asserted against GORM's `TagSettings["JSON"]`, but `TagSettings` only parses the `gorm:` tag (verified against gorm v1.31.1 source), so the test passed even with `json:"-"` deleted (proven by mutation test). It now asserts `field.Tag.Get("json") == "-"` and is proven to FAIL when the tag is removed. Full re-verification after the fix: build/vet/`go test -count=1`/`go test -race -count=1` all pass; integration test passes against live PostgreSQL (5432) with fixture cleanup confirmed (0 residual rows); live server verified `/health` 200, `/auth/github/login` 302 with PKCE S256 + state cookie, callback without state → `invalid_state`, `/users/me` → uniform `401`; startup AutoMigrate left the physical schema unchanged (`git_hub_access_token` intact, no migration needed).

## Completed (2026-08-23 — Task 2a GitHub repository discovery)

- **Task 2a — `GET /repositories` (complete, verified 2026-08-23):** authenticated endpoint returning the current Aevor user's GitHub repositories through their stored encrypted access token. Discovery only — no persistence, no caching, no AI, no frontend changes.
  - Files: `internal/github/client.go` (+`Repository`/`RepositoryOwner` types, `ListUserRepositories`, `hasNextPage`), `internal/users/service.go` (+`DecryptedGitHubToken`, `ErrGitHubTokenMissing`), NEW package `internal/repositories/{service,dto,handler}.go`, `cmd/server/main.go` (route wiring).
  - Architecture preserved: Handler → Service → GitHub Client → GitHub API; identity solely from `RequireAuth` JWT (`GetAuthenticatedUserID`) — query params/headers can never select another user's credentials (tested).
  - Contract: `GET /repositories?page=&per_page=` (Bearer Aevor JWT). Defaults page=1, per_page=30, per_page clamped to 100, invalid values → 400 `invalid_pagination`. Response `{repositories:[{id,name,full_name,description,private,default_branch,owner_login,html_url,clone_url,api_url}],page,per_page,has_more}`. GitHub call uses GET `/user/repos?sort=full_name&direction=asc` (deterministic order), pagination via Link header (`has_more`).
  - Error surface: 401 `unauthorized` (no/garbage JWT); 404 `user_not_found`; 403 `github_token_missing`; 401 `github_token_invalid` (GitHub rejects stored token — remedy is re-login); 429 `github_rate_limited`; 500 `github_unavailable` (GitHub down/malformed payload/other API errors); 500 `internal` (decryption failure etc., no crypto detail leaked).
  - Tests: `client_repositories_test.go` (request shape incl. deterministic sort + Bearer + API version, mapping, empty list, Link-header has_more true/false, status taxonomy, network failure, malformed/negative-id/blank-full-name rejection, token never in errors), `service_token_test.go` (decrypt round-trip, user-not-found, nil/blank/empty token, corrupt ciphertext, wrong key, short key), `repositories/handler_test.go` (unauthenticated 401 with zero GitHub contact, success mapping + pagination echo, token plaintext AND ciphertext absent from response body, per_page clamp to 100 forwarded, empty array not null, invalid pagination matrix, user_not_found/token_missing/decrypt-failure/GitHub-failure mappings, identity-from-JWT-only vs `?user_id=`, request-context propagation).
  - Verified live (local server + real DB): `/health` 200; `/repositories` unauthenticated and garbage-bearer → uniform 401; fixture user with NULL token → 403 `github_token_missing`; fixture user with encrypted dummy token → GitHub rejects → 401 `github_token_invalid` (full chain proven: JWT → user lookup → decrypt → GitHub call → error map); fixture rows cleaned (0 residue).
  - Deferred finding (pre-existing): `pkg/database/postgres.go` uses GORM's default logger, so SQL errors log full statements including bound ciphertext — tighten logging as a separate task.

## Current

- **Task 1 close-out:**
  - DONE: repairs implemented, reviewed, tested, and committed on branch `fix/users-upsert-token-column` (`098dd4d` fix+regression tests, `a8b4479` jwt deflake, docs commit).
  - BLOCKED (human step): manual end-to-end OAuth re-login in a real browser — exercises the fixed conflict path live (DB level already proven by the integration test).
  - TODO (Sanjeev): push the branch, open PR, review, approve, merge.

## Next

- **Task 2 — GitHub data synchronization:** repository details, GitHub issues, commits, pull requests (uses the stored encrypted GitHub access token via a server-side retrieval + decrypt path).

## Future

- skills
- recommendations
- embeddings
- semantic analysis
- Ollama integration
- background workers
- frontend repository UI
