#!/bin/bash
# Migration runner script for Aevor platform
# This script runs database migrations in a safe, controlled manner

set -euo pipefail

# Configuration
MIGRATION_DIR="${MIGRATION_DIR:-/app/migrations}"
DB_HOST="${DB_HOST:-postgres}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-aevor}"
DB_PASSWORD="${DB_PASSWORD}"
DB_NAME="${DB_NAME:-aevor}"
DB_SSLMODE="${DB_SSLMODE:-disable}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Wait for database to be ready
wait_for_db() {
    log_info "Waiting for database to be ready..."
    local max_attempts=30
    local attempt=1
    while ! pg_isready -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" >/dev/null 2>&1; do
        if [ $attempt -ge $max_attempts ]; then
            log_error "Database not ready after $max_attempts attempts"
            exit 1
        fi
        log_info "Attempt $attempt/$max_attempts: Database not ready, waiting..."
        sleep 2
        attempt=$((attempt + 1))
    done
    log_info "Database is ready"
}

# Run migrations
run_migrations() {
    log_info "Running database migrations..."

    # Check if migration directory exists
    if [ ! -d "$MIGRATION_DIR" ]; then
        log_warn "Migration directory $MIGRATION_DIR does not exist, skipping migrations"
        return 0
    fi

    # Find all migration files (sorted)
    local migrations=($(find "$MIGRATION_DIR" -name "*.sql" -o -name "*.up.sql" | sort))
    if [ ${#migrations[@]} -eq 0 ]; then
        log_warn "No migration files found in $MIGRATION_DIR"
        return 0
    fi

    log_info "Found ${#migrations[@]} migration(s)"

    # Create migrations table if it doesn't exist
    PGPASSWORD="$DB_PASSWORD" psql \
        -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" \
        -c "CREATE TABLE IF NOT EXISTS schema_migrations (version VARCHAR(255) PRIMARY KEY, applied_at TIMESTAMP WITH TIME ZONE DEFAULT NOW());"

    for migration in "${migrations[@]}"; do
        version=$(basename "$migration" .sql)
        version=$(basename "$version" .up.sql)

        # Check if already applied
        applied=$(PGPASSWORD="$DB_PASSWORD" psql \
            -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" \
            -tAc "SELECT 1 FROM schema_migrations WHERE version = '$version';")

        if [ "$applied" = "1" ]; then
            log_info "Migration $version already applied, skipping"
            continue
        fi

        log_info "Applying migration: $version"
        if PGPASSWORD="$DB_PASSWORD" psql \
            -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" \
            -f "$migration"; then
            PGPASSWORD="$DB_PASSWORD" psql \
                -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" \
                -c "INSERT INTO schema_migrations (version) VALUES ('$version');"
            log_info "Migration $version applied successfully"
        else
            log_error "Migration $version failed"
            exit 1
        fi
    done

    log_info "All migrations applied successfully"
}

# Verify migrations
verify_migrations() {
    log_info "Verifying migrations..."
    local applied_count=$(PGPASSWORD="$DB_PASSWORD" psql \
        -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" \
        -tAc "SELECT COUNT(*) FROM schema_migrations;")
    log_info "Applied migrations: $applied_count"
}

# Main
main() {
    log_info "Starting Aevor database migration process"
    wait_for_db
    run_migrations
    verify_migrations
    log_info "Migration process completed successfully"
}

main "$@"