#!/bin/bash
# Restore script for Aevor PostgreSQL database
# This script restores a database from a backup file

set -euo pipefail

# Configuration
DB_HOST="${DB_HOST:-postgres}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-aevor}"
DB_PASSWORD="${DB_PASSWORD}"
DB_NAME="${DB_NAME:-aevor}"
BACKUP_DIR="${BACKUP_DIR:-/backups}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $(date '+%Y-%m-%d %H:%M:%S') $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $(date '+%Y-%m-%d %H:%M:%S') $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $(date '+%Y-%m-%d %H:%M:%S') $1"
}

# Check if backup file is provided
if [ $# -eq 0 ]; then
    log_error "Usage: $0 <backup_file>"
    log_info "Available backups:"
    ls -la /backups/aevor_${DB_NAME}_*.sql* 2>/dev/null || log_warn "No backups found"
    exit 1
fi

BACKUP_FILE="$1"

# Check if backup file exists
if [ ! -f "$BACKUP_FILE" ]; then
    # Try looking in the backup directory
    if [ -f "${BACKUP_DIR}/${BACKUP_FILE}" ]; then
        BACKUP_FILE="${BACKUP_DIR}/${BACKUP_FILE}"
    else
        log_error "Backup file not found: $BACKUP_FILE"
        exit 1
    fi
fi

log_warn "This will REPLACE the current database '$DB_NAME' with the backup!"
log_warn "Backup file: $BACKUP_FILE"
log_warn "This operation CANNOT be undone!"

# Confirm unless FORCE is set
if [ "${FORCE:-false}" != "true" ]; then
    read -p "Are you sure you want to continue? (yes/no): " CONFIRM
    if [ "$CONFIRM" != "yes" ]; then
        log_info "Restore cancelled"
        exit 0
    fi
fi

log_info "Starting restore of database '$DB_NAME' from '$BACKUP_FILE'"

# Check if file is compressed
if [[ "$BACKUP_FILE" == *.gz ]]; then
    log_info "Decompressing and restoring..."
    gunzip -c "$BACKUP_FILE" | PGPASSWORD="$DB_PASSWORD" psql \
        -h "$DB_HOST" \
        -p "$DB_PORT" \
        -U "$DB_USER" \
        -d "$DB_NAME" \
        -v ON_ERROR_STOP=1
else
    log_info "Restoring..."
    PGPASSWORD="$DB_PASSWORD" psql \
        -h "$DB_HOST" \
        -p "$DB_PORT" \
        -U "$DB_USER" \
        -d "$DB_NAME" \
        -v ON_ERROR_STOP=1 \
        -f "$BACKUP_FILE"
fi

if [ ${PIPESTATUS[0]} -eq 0 ]; then
    log_info "Restore completed successfully"
else
    log_error "Restore failed"
    exit 1
fi

# Verify restore by checking table count
TABLE_COUNT=$(PGPASSWORD="$DB_PASSWORD" psql \
    -h "$DB_HOST" \
    -p "$DB_PORT" \
    -U "$DB_USER" \
    -d "$DB_NAME" \
    -tAc "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public';")

log_info "Restore verification: $TABLE_COUNT tables in database"
log_info "Restore process completed successfully"