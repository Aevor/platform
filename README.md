# Aevor Platform

The Go API behind Aevor, a review-first developer tool for working through GitHub issues and repository changes. This repository owns the backend; the React client lives in [Aevor/Frontend](https://github.com/Aevor/Frontend).

The API connects a user's GitHub account, gives them access to selected repositories, builds bounded code context, and supports an issue-to-change workflow. A proposed change is not published just because an AI service produced it: the user has to approve the change set, the applied result is validated, and publishing is a separate step.

## What is implemented here

- **Account and GitHub access:** GitHub OAuth with PKCE, Aevor JWT sessions, and encrypted storage for the GitHub access token.
- **Repository workspace:** list and select repositories, sync issues / pull requests / commits, and clone selected repositories into a controlled workspace. Clone URLs are restricted by an allowlist; local `file://` transport is opt-in.
- **Code context:** discover files, apply bounded filtering, extract content, split it into chunks, and create traceable code representations.
- **Index and analysis:** query an in-memory metadata index, analyze repository context, inspect potential impact, and retrieve saved analyses.
- **Issue workflow:** analyze an issue, propose a solution, generate a reviewable change set, approve it, apply it to the workspace, validate it, and explicitly publish a branch / commit / pull request.
- **Pull-request review:** retrieve pull-request details and run a separate analysis of review feedback.
- **Supporting endpoints:** health and readiness checks, contribution summary, skills, recommendations, and asynchronous job handling.

The route registration in `services/api/cmd/server/main.go` is the source of truth for the current HTTP surface. This is an actively developed API, not a claim that the whole product is deployed or production-ready.

## Architecture

```text
React client (Aevor/Frontend)
        |
        | HTTP + Aevor JWT
        v
Go API (this repository)
  ├── auth / users
  ├── GitHub API client
  ├── repository and workspace services
  ├── discovery → filtering → extraction
  │                → chunking → representation
  ├── metadata index and impact analysis
  ├── issue / change-set / publishing workflow
  └── jobs and webhook handling
        |
        ├── PostgreSQL
        ├── GitHub
        └── separately configured AI service
```

The metadata index is currently in memory, so it is rebuilt rather than treated as durable storage after an API restart. AI-backed routes need a reachable service configured through `AI_SERVICE_URL`; the API repository does not bundle that model service.

## Run locally

Requirements: Go 1.26.1 or newer, Docker with Compose, and a GitHub OAuth App.

From the repository root, start PostgreSQL:

```sh
docker compose -f deployments/docker/docker-compose.yml up -d
```

Configure the API:

```sh
cd services/api
cp .env.example .env
```

Fill in the required database values, GitHub OAuth client ID / secret and callback URL, `FRONTEND_URL`, `WORKSPACE_ROOT`, `JWT_SECRET`, and `GITHUB_TOKEN_ENCRYPTION_KEY`.

Generate the two cryptographic keys independently:

```sh
openssl rand -hex 32
openssl rand -hex 32
```

Use the first value for `JWT_SECRET` and the second for `GITHUB_TOKEN_ENCRYPTION_KEY`. Do not reuse a key for both purposes. The OAuth callback should match the configured `GITHUB_REDIRECT_URL`, normally `http://localhost:8080/auth/github/callback`.

Run the API from `services/api`:

```sh
go run ./cmd/server
```

The API listens on port 8080 by default. The frontend's local Vite server normally runs on port 5173; set `FRONTEND_URL` to that address.

## Validation

Run from `services/api`:

```sh
gofmt -w .
go vet ./...
go test -race ./...
go build ./...
```

The opt-in PostgreSQL integration tests require `AEVOR_TEST_DATABASE_DSN`. Unit tests should run without a live GitHub or AI service by using the injected test clients and fakes.

## Security boundaries worth knowing

- User identity comes from the verified Aevor JWT, not from a caller-supplied user ID.
- GitHub access tokens are encrypted before they are stored.
- Repository clones are restricted to configured hosts; local file transport is disabled unless explicitly enabled.
- Repository processing has file, byte, chunk and result limits.
- The indexing and representation endpoints return bounded metadata rather than raw source content.
- Generating a proposal does not publish it. Approval, apply, validation and publishing are distinct operations.

## Development notes

- `docs/development-state.md` and `docs/todo.md` are dated implementation notes, not live status dashboards.
- Product and architecture documents are maintained separately in [Aevor/docs](https://github.com/Aevor/docs).
