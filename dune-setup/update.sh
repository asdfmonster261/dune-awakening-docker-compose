#!/bin/bash
# update.sh — pull the latest Dune Awakening self-host release from Steam
# (app 4754530) and apply it to the running docker-compose stack.
#
# Usage:
#   ./update.sh check     - download latest, compare against installed, exit
#   ./update.sh apply     - check + pre-update backup + load + tag swap +
#                           restart + verify; pauses for rollback on failure
#   ./update.sh verify    - re-run the post-update health checks
#   ./update.sh rollback  - revert to the previous tag + restore the pre-update
#                           snapshot. Reads state from .last-update.
#
# Steam downloads land in $REPO_ROOT/.updates/ (staging). Pre-update DB
# snapshots go into the postgres-backup volume with a "pre-update-" prefix
# so the sidecar's rolling retention doesn't auto-prune them.
set -eu

G_SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$G_SCRIPT_PATH/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.yml"
STAGING_PATH="$REPO_ROOT/.updates"
SERVER_PATH="$REPO_ROOT/server"
STATE_FILE="$REPO_ROOT/.last-update"
APP_ID="4754530"

RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; NC=$'\033[0m'

# ────────────────────────────────────────────────────────────────────────────
# Helpers
# ────────────────────────────────────────────────────────────────────────────

require_cmd() {
    for c in "$@"; do
        if ! command -v "$c" >/dev/null 2>&1; then
            echo "${RED}Missing required command: $c${NC}" >&2
            exit 1
        fi
    done
}

# Read the current Funcom image tag from docker-compose.yml. Uses
# bg-director's line as the canonical source — all the game services
# share the same tag.
current_image_tag() {
    grep -oE 'seabass-server-bg-director:[A-Za-z0-9._\-]+' "$COMPOSE_FILE" \
        | head -1 | sed 's/.*://'
}

# Read the staged version (from the downloaded version.txt).
staged_version() {
    local f="$STAGING_PATH/images/battlegroup/version.txt"
    [ -f "$f" ] && tr -d '\n' < "$f"
}

# compose helper that always uses the right project dir
dc() {
    docker compose --project-directory "$REPO_ROOT" "$@"
}

# ────────────────────────────────────────────────────────────────────────────
# Step functions
# ────────────────────────────────────────────────────────────────────────────

download_to_staging() {
    mkdir -p "$STAGING_PATH"
    echo "Downloading latest via steamcmd (incremental; may take a while on first run)..."
    # Steamcmd quirk: under `set -e`, a non-zero exit kills us silently.
    # Capture it so we can show a useful error.
    local rc=0
    steamcmd \
        +set_spew_level 1 1 \
        +force_install_dir "$STAGING_PATH" \
        +login anonymous \
        +app_update "$APP_ID" \
        +logoff +quit || rc=$?
    if [ $rc -ne 0 ]; then
        echo "${RED}steamcmd failed (exit $rc)${NC}" >&2
        exit $rc
    fi
}

show_diff_summary() {
    local current="$1" new="$2"
    echo
    echo "Version: ${current} → ${new}"

    # Heads-up if Funcom changed any of the config/template files we layered
    # our own overrides on top of.
    local files=(
        "scripts/setup/templates/world-template.yaml"
    )
    local any_changed=0
    for f in "${files[@]}"; do
        local old_file="$SERVER_PATH/$f"
        local new_file="$STAGING_PATH/$f"
        if [ -f "$old_file" ] && [ -f "$new_file" ]; then
            if ! diff -q "$old_file" "$new_file" >/dev/null 2>&1; then
                echo "${YELLOW}⚠  $f changed; diff:${NC}  diff $old_file $new_file"
                any_changed=1
            fi
        fi
    done

    # Heads-up if the orchestrator might need re-investigation. The four
    # operator binaries are how Funcom encodes the BG CR contract; if any of
    # them changed, the BG CR shape probably did too.
    if [ -d "$STAGING_PATH/images/operators" ] && [ -d "$SERVER_PATH/images/operators" ]; then
        local old_v new_v
        old_v=$(cat "$SERVER_PATH/images/operators/version.txt" 2>/dev/null || echo "?")
        new_v=$(cat "$STAGING_PATH/images/operators/version.txt" 2>/dev/null || echo "?")
        if [ "$old_v" != "$new_v" ]; then
            echo "${YELLOW}⚠  operator version changed ($old_v → $new_v); the orchestrator's${NC}"
            echo "${YELLOW}   K8s API emulation may need updates if the BG CR shape changed${NC}"
            any_changed=1
        fi
    fi

    if [ $any_changed -eq 0 ]; then
        echo "(no upstream config drift detected)"
    fi
}

pre_update_backup() {
    local ts snapshot
    ts=$(date -u +%Y%m%d-%H%M%S)
    snapshot="/backups/pre-update-${ts}.sql.gz"
    echo "Taking pre-update backup → $snapshot ..."
    # --clean + --if-exists so the restore script can run against a non-empty
    # database without prep. Different from the sidecar's vanilla pg_dump.
    dc exec -T postgres-backup sh -c "
        pg_dump --clean --if-exists -h postgres -U dune -d dune | gzip > $snapshot
    "
    echo "${GREEN}Backup done${NC}"
    PRE_UPDATE_SNAPSHOT="$snapshot"
}

load_images_from_staging() {
    echo "Loading new images..."
    local img count=0
    # battlegroup tarballs (the .NET + UE servers + RMQ + DB utils)
    for img in "$STAGING_PATH"/images/battlegroup/*.tar; do
        [ -f "$img" ] || continue
        echo "  $(basename "$img")"
        docker load -i "$img" >/dev/null
        count=$((count + 1))
    done
    # postgres prerequisite
    if [ -f "$STAGING_PATH/images/prerequisites/igw-postgres.tar" ]; then
        echo "  igw-postgres.tar"
        docker load -i "$STAGING_PATH/images/prerequisites/igw-postgres.tar" >/dev/null
        count=$((count + 1))
    fi
    echo "${GREEN}Loaded $count image(s)${NC}"
}

update_compose_tags() {
    local current="$1" new="$2"
    cp "$COMPOSE_FILE" "$COMPOSE_FILE.bak"
    # Strict: only touch lines that reference Funcom's registry. Leaves
    # dune-orchestrator:latest, dune-admin:latest, filebrowser, etc. alone.
    sed -i -E "s|(registry\\.funcom\\.com/funcom/self-hosting/[^:]+):${current}|\\1:${new}|g" \
        "$COMPOSE_FILE"
    echo "${GREEN}Compose tags updated (backup: ${COMPOSE_FILE}.bak)${NC}"
}

restart_stack() {
    echo "Restarting stack..."
    dc up -d
}

verify() {
    local timeout="${1:-180}"
    local elapsed=0
    echo "Verifying ($timeout s timeout)..."

    # 1. bg-director: "Initialize connection to FLS environment"
    while [ $elapsed -lt $timeout ]; do
        if dc logs --no-color --since 5m bg-director 2>&1 \
           | grep -q "Initialize connection to FLS environment"; then
            echo "${GREEN}  ✓ bg-director initialized to FLS${NC}"
            break
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    if [ $elapsed -ge $timeout ]; then
        echo "${RED}  ✗ bg-director did not initialize within ${timeout}s${NC}"
        return 1
    fi

    # 2. At least one game-server reaches ready=true in the orchestrator's view
    local extra=0
    local extra_max=120
    while [ $extra -lt $extra_max ]; do
        if dc logs --no-color --since 5m dune-orchestrator 2>&1 \
           | grep -q "ready=true.*phase=Running"; then
            echo "${GREEN}  ✓ at least one game-server reached ready=true${NC}"
            return 0
        fi
        sleep 5
        extra=$((extra + 5))
    done
    echo "${RED}  ✗ no game-server reached ready=true within ${extra_max}s${NC}"
    return 1
}

# Persist the data needed to roll back via a subsequent `./update.sh rollback`.
write_state() {
    local snapshot="$1" old_tag="$2" new_tag="$3"
    cat > "$STATE_FILE" <<EOF
# Written by update.sh on $(date -Iseconds)
PRE_UPDATE_SNAPSHOT="$snapshot"
OLD_TAG="$old_tag"
NEW_TAG="$new_tag"
EOF
    chmod 600 "$STATE_FILE"
}

pause_for_rollback() {
    local current="$1" new="$2"
    echo
    echo "${RED}=== UPDATE VERIFY FAILED ===${NC}"
    echo
    echo "Common next steps:"
    echo "  ${YELLOW}Inspect logs:${NC}"
    echo "    docker compose logs --tail=200 bg-director"
    echo "    docker compose logs --tail=200 game-server-survival"
    echo
    echo "  ${YELLOW}Wait longer + re-verify:${NC}"
    echo "    ./update.sh verify"
    echo
    echo "  ${YELLOW}Roll back to $current${NC} (restores ${PRE_UPDATE_SNAPSHOT:-?}):"
    echo "    ./update.sh rollback"
    echo
    read -r -p "Roll back now? [y/N]: " yn
    case "$yn" in
        [yY]*)
            cmd_rollback
            ;;
        *)
            echo "Paused. Re-run with './update.sh rollback' when ready, or"
            echo "'./update.sh verify' if you want to wait longer."
            exit 2
            ;;
    esac
}

# ────────────────────────────────────────────────────────────────────────────
# Subcommands
# ────────────────────────────────────────────────────────────────────────────

cmd_check() {
    require_cmd steamcmd docker
    download_to_staging

    local current new
    current=$(current_image_tag)
    new=$(staged_version)

    if [ -z "$new" ]; then
        echo "${RED}Staging is missing version.txt; download may have failed${NC}" >&2
        exit 1
    fi

    if [ "$current" = "$new" ]; then
        echo "${GREEN}Up to date: $current${NC}"
        return 0
    fi

    echo "${YELLOW}Update available${NC}"
    show_diff_summary "$current" "$new"
    # Exit non-zero so scripted callers can detect "update available"
    return 1
}

cmd_apply() {
    require_cmd steamcmd docker

    # Reuse cmd_check for download + summary. cmd_check returns 1 when an
    # update is available; treat that as success here.
    local check_rc=0
    cmd_check || check_rc=$?
    case $check_rc in
        0)
            echo "Nothing to do."
            exit 0
            ;;
        1) ;;
        *) exit $check_rc ;;
    esac

    local current new
    current=$(current_image_tag)
    new=$(staged_version)

    echo
    read -r -p "Proceed with applying ${current} → ${new}? [y/N]: " yn
    case "$yn" in
        [yY]*) ;;
        *) echo "Aborted."; exit 0;;
    esac

    pre_update_backup
    # Persist state before any destructive change so `./update.sh rollback`
    # has everything it needs even if a later step blows up.
    write_state "$PRE_UPDATE_SNAPSHOT" "$current" "$new"
    load_images_from_staging
    update_compose_tags "$current" "$new"
    restart_stack

    if verify; then
        echo
        echo "${GREEN}✓ Update applied successfully${NC}"
        echo "  ${current} → ${new}"
        echo "  Pre-update backup: $PRE_UPDATE_SNAPSHOT"
        echo "  Old compose: ${COMPOSE_FILE}.bak (delete when satisfied)"
    else
        pause_for_rollback "$current" "$new"
    fi
}

cmd_verify() {
    if verify "${1:-180}"; then
        echo "${GREEN}✓ Verify passed${NC}"
    else
        exit 1
    fi
}

cmd_rollback() {
    if [ ! -f "$STATE_FILE" ]; then
        echo "${RED}No $STATE_FILE found; can't determine rollback target.${NC}" >&2
        echo "If you have a known-good pre-update snapshot, restore manually:" >&2
        echo "  docker compose down" >&2
        echo "  # restore compose to old tag (edit docker-compose.yml manually)" >&2
        echo "  gunzip -c /var/lib/docker/volumes/dune-server_postgres-backups/_data/pre-update-XXX.sql.gz | \\" >&2
        echo "    docker compose exec -T postgres psql -U dune -d dune" >&2
        echo "  docker compose up -d" >&2
        exit 1
    fi
    # shellcheck disable=SC1090
    . "$STATE_FILE"

    echo "Rollback plan:"
    echo "  Compose tag:  $NEW_TAG → $OLD_TAG"
    echo "  DB restore:   $PRE_UPDATE_SNAPSHOT"
    echo
    read -r -p "${RED}This will overwrite the current postgres data.${NC} Proceed? [y/N]: " yn
    case "$yn" in
        [yY]*) ;;
        *) echo "Aborted."; exit 0;;
    esac

    # Restore the compose tag. If a .bak exists from the apply step, use it
    # (preserves any nearby formatting we might have touched); else sed.
    if [ -f "$COMPOSE_FILE.bak" ]; then
        mv "$COMPOSE_FILE.bak" "$COMPOSE_FILE"
        echo "${GREEN}Restored $COMPOSE_FILE from .bak${NC}"
    else
        sed -i -E "s|(registry\\.funcom\\.com/funcom/self-hosting/[^:]+):${NEW_TAG}|\\1:${OLD_TAG}|g" \
            "$COMPOSE_FILE"
        echo "${GREEN}Reverted compose tag via sed${NC}"
    fi

    # Stop game servers + director so DB restore doesn't conflict with live
    # writers. Leave postgres + postgres-backup up so we can stream the dump in.
    echo "Stopping game/director containers..."
    dc stop bg-director server-gateway text-router \
        game-server-survival game-server-overmap 2>/dev/null || true

    # Restore. The pre-update snapshot used pg_dump --clean --if-exists, so it
    # drops + recreates everything in the dune schema cleanly.
    echo "Restoring $PRE_UPDATE_SNAPSHOT ..."
    dc exec -T postgres-backup sh -c "
        gunzip -c $PRE_UPDATE_SNAPSHOT | PGPASSWORD=\$PGPASSWORD psql -h postgres -U dune -d dune
    "

    echo "Restarting stack..."
    dc up -d

    if verify 180; then
        echo "${GREEN}✓ Rollback complete and verified${NC}"
        rm -f "$STATE_FILE"
    else
        echo "${RED}Rollback applied but verify failed; manual investigation needed.${NC}" >&2
        exit 1
    fi
}

# ────────────────────────────────────────────────────────────────────────────

main() {
    case "${1:-}" in
        check)    cmd_check ;;
        apply)    cmd_apply ;;
        verify)   shift; cmd_verify "$@" ;;
        rollback) cmd_rollback ;;
        ""|-h|--help|help)
            cat <<EOF
Usage: $0 {check|apply|verify|rollback}

  check     Download latest from Steam, report version delta. No changes.
  apply     check + pre-update DB backup + docker load + compose tag swap +
            restart + verify. Pauses with rollback prompt on verify failure.
  verify    Re-run the post-update health checks (useful when boots are slow).
  rollback  Revert to the previous tag and restore the pre-update DB snapshot.
            Uses state recorded in ${STATE_FILE}.
EOF
            ;;
        *)
            echo "Unknown subcommand: $1" >&2
            exit 1
            ;;
    esac
}

main "$@"
