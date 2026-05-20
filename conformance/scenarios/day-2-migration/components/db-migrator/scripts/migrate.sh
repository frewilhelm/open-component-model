#!/bin/sh
set -e

DB_HOST="${DB_HOST:-postgres}"
DB_PORT="${DB_PORT:-5432}"
DB_NAME="${DB_NAME:-day2}"
DB_USER="${DB_USER:-day2}"

export PGPASSWORD="${DB_PASSWORD:-changeme}"

echo "Waiting for PostgreSQL at ${DB_HOST}:${DB_PORT}..."
until pg_isready -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -q; do
  sleep 1
done
echo "PostgreSQL is ready."

PSQL="psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -v ON_ERROR_STOP=1"

# Create schema_migrations tracking table
$PSQL -c "CREATE TABLE IF NOT EXISTS schema_migrations (
  id SERIAL PRIMARY KEY,
  filename TEXT NOT NULL UNIQUE,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);"

# Run migrations in order, skip already-applied
for f in /migrations/*.sql; do
  [ -f "$f" ] || continue
  filename=$(basename "$f")
  applied=$($PSQL -t -c "SELECT COUNT(*) FROM schema_migrations WHERE filename='$filename';" | tr -d ' ')
  if [ "$applied" = "0" ]; then
    echo "Applying: $filename"
    $PSQL -f "$f"
    $PSQL -c "INSERT INTO schema_migrations (filename) VALUES ('$filename');"
  else
    echo "Skipping (already applied): $filename"
  fi
done

echo ""
echo "=== Applied migrations ==="
$PSQL -c "SELECT filename, applied_at FROM schema_migrations ORDER BY id;"
echo ""
echo "=== Current schema ==="
$PSQL -c "\dt"
$PSQL -c "\d items"
echo ""
echo "Migration complete."
