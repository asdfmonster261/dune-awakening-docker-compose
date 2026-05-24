#!/bin/bash
# Load all required Funcom image tarballs into Docker.
# Idempotent — `docker load` is a no-op if the image already exists.
set -euo pipefail

G_SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMG_DIR="$G_SCRIPT_PATH/../server/images"

images=(
    "$IMG_DIR/prerequisites/igw-postgres.tar"
    "$IMG_DIR/battlegroup/server-db-utils.tar"
    "$IMG_DIR/battlegroup/server-rabbitmq.tar"
    "$IMG_DIR/battlegroup/server-text-router.tar"
    "$IMG_DIR/battlegroup/server-bg-director.tar"
    "$IMG_DIR/battlegroup/server-gateway.tar"
    "$IMG_DIR/battlegroup/server.tar"
)

for tar in "${images[@]}"; do
    if [ ! -f "$tar" ]; then
        echo "Missing image tarball: $tar" >&2
        exit 1
    fi
    echo "Loading $(basename "$tar")..."
    docker load -i "$tar"
done

echo "All Funcom images loaded."
echo "Filebrowser image will be pulled from Docker Hub on first compose up."
