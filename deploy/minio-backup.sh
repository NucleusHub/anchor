#!/usr/bin/env bash
#
# Daily Nucleus backup — MinIO object store + MongoDB, grouped per run.
#
# Layout (one bundle directory per run):
#   $BACKUP_DIR/<YYYYMMDD-HHMMSS>/
#     minio.tar.gz   # the whole MinIO data volume
#     mongo.gz       # mongodump --archive --gzip of the nucleus database
#
# Keeps the newest $KEEP bundles. Anchor's "Backups" tab lists these and can
# restore one: it rolls back MinIO AND only Orbit's Mongo collections
# (orbitfiles, orbitfolders), leaving every other app's data untouched.
#
# Install via cron at 00:00 daily:
#   0 0 * * * /home/debian/scripts/minio-backup.sh >> /home/debian/backups/minio/backup.log 2>&1
#
# Env overrides: BACKUP_DIR, MINIO_VOLUME, MONGO_CONTAINER, MONGO_DB, KEEP.
#
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

BACKUP_DIR="${BACKUP_DIR:-/home/debian/backups/minio}"
MINIO_VOLUME="${MINIO_VOLUME:-nucleus_minio_data}"
MONGO_CONTAINER="${MONGO_CONTAINER:-nucleus-mongo-1}"
MONGO_DB="${MONGO_DB:-nucleus}"
KEEP="${KEEP:-3}"

STAMP="$(date +%Y%m%d-%H%M%S)"
WORK="$BACKUP_DIR/.${STAMP}.partial"
DEST="$BACKUP_DIR/$STAMP"
mkdir -p "$WORK"

# MinIO data volume — read-only helper mount so a backup can't touch live data.
docker run --rm -v "${MINIO_VOLUME}:/data:ro" -v "${WORK}:/out" alpine:latest \
  tar czf /out/minio.tar.gz -C /data .

# MongoDB — whole nucleus DB as a gzipped archive (restore filters to Orbit).
docker exec "$MONGO_CONTAINER" mongodump --db="$MONGO_DB" --archive --gzip > "${WORK}/mongo.gz"

# Publish the bundle only once both halves succeeded.
mv "$WORK" "$DEST"
echo "$(date '+%F %T') OK  ${STAMP}  (minio $(du -h "${DEST}/minio.tar.gz" | cut -f1), mongo $(du -h "${DEST}/mongo.gz" | cut -f1))"

# Retention: keep the newest $KEEP bundle directories; drop the rest. Also clear
# any stale .partial dirs from interrupted runs.
rm -rf "$BACKUP_DIR"/.*.partial 2>/dev/null || true
ls -1dt "$BACKUP_DIR"/*/ 2>/dev/null | grep -E '/[0-9]{8}-[0-9]{6}/$' | tail -n +$((KEEP + 1)) | xargs -r rm -rf || true
