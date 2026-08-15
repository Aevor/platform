# Aevor Platform

Core platform repository for Aevor — a developer analytics platform backed by GitHub OAuth.

## Components

- **API Services** (`services/api`) — Go (Gin) service: GitHub OAuth 2.0 + PKCE login, JWT-session auth, `/users/me`, GitHub data sync (planned).
- **Web Application** (planned) — frontend repository UI.
- **Shared Packages** — cross-service utilities.
- **Deployments** (`deployments/docker`) — local Postgres via docker-compose.

## Getting started

### Prerequisites

- Go 1.22+
- Docker (for Postgres)

### 1. Start Postgres

```sh
docker compose -f deployments/docker/docker-compose.yml up -d
```

### 2. Configure the API

```sh
cp services/api/.env.example services/api/.env
# fill in GITHUB_CLIENT_ID / GITHUB_CLIENT_SECRET / JWT_SECRET / GITHUB_TOKEN_ENCRYPTION_KEY
```

The service loads `.env` from its working directory.

### 3. Run the API

```sh
cd services/api
go run ./cmd/server
```

Endpoints:

- `GET /health` — liveness
- `GET /auth/github/login` — start OAuth login
- `GET /auth/github/callback` — OAuth callback (issues JWT)
- `GET /users/me` — current user (`Authorization: Bearer <jwt>`)

### Tests

```sh
cd services/api
go test -race ./...
```

## Docs

- `docs/development-state.md` — development state snapshot
- `docs/todo.md` — task tracker
