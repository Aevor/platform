#!/bin/bash
# Backup script for Aevor PostgreSQL database
# This script creates a consistent backup of the database

set -euo pipefail

# Configuration
DB_HOST="${DB_HOST:-postgres}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-aevor}"
DB_PASSWORD="${DB_PASSWORD}"
DB_NAME="${DB_NAME:-aevor}"
BACKUP_DIR="${BACKUP_DIR:-/backups}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"
COMPRESS="${COMPRESS:-true}"

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

# Create backup directory
mkdir -p "$BACKUP_DIR"

# Generate backup filename with timestamp
TIMESTAMP=$(date '+%Y%m%d_%H%M%S')
BACKUP_FILE="${BACKUP_DIR}/aevor_${DB_NAME}_${TIMESTAMP}.sql"

if [ "$COMPRESS" = "true" ]; then
    BACKUP_FILE="${BACKUP_FILE}.gz"
fi

log_info "Starting backup of database '$DB_NAME' to '$BACKUP_FILE'"

# Run pg_dump
if [ "$COMPRESS" = "true" ]; then
    PGPASSWORD="$DB_PASSWORD" pg_dump \
        -h "$DB_HOST" \
        -p "$DB_PORT" \
        -U "$DB_USER" \
        -d "$DB_NAME" \
        --no-owner \
        --no-privileges \
        --clean \
        --if-exists \
        | gzip > "$BACKUP_FILE"
else
    PGPASSWORD="$DB_PASSWORD" pg_dump \
        -h "$DB_HOST" \
        -p "$DB_PORT" \
        -U "$DB_USER" \
        -d "$DB_NAME" \
        --no-owner \
        --no-privileges \
        --clean \
        --if-exists \
        > "$BACKUP_FILE"
fi

# Check if backup was successful
if [ ${PIPESTATUS[0]} -eq 0 ]; then
    BACKUP_SIZE=$(du -h "$BACKUP_FILE" | cut -f1)
    log_info "Backup completed successfully: $BACKUP_FILE ($BACKUP_SIZE)"
else
    log_error "Backup failed"
    exit 1
fi

# Clean up old backups
log_info "Cleaning up backups older than $RETENTION_DAYS days"
find "$BACKUP_DIR" -name "aevor_${DB_NAME}_*.sql*" -mtime +$RETENTION_DAYS -delete
REMAINING=$(find "$BACKUP_DIR" -name "aevor_${DB_NAME}_*.sql*" | wc -l)
log_info "Cleanup complete. Remaining backups: $REMAINING"

log_info "Backup process completed successfully"