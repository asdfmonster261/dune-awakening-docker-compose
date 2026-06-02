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

    # Two more places have the version baked in. If we skip these, the
    # gateway registers the battlegroup with FLS at the old revision and
    # rejects the new game-servers as "mismatching revision".
    bump_version_sidecars "$current" "$new"
}

# The gateway reads `revision` from gateway-override.ini at startup and
# reports it to FLS; the orchestrator-managed pods use the image tags in
# world.yaml. Both files are RENDERED at install time and never
# regenerated, so update.sh has to patch them in place.
#
# Tags here look like '1968181-0-shipping'. gateway-override.ini's
# revision key uses just the numeric prefix; world.yaml uses the full tag.
bump_version_sidecars() {
    local current="$1" new="$2"
    local current_rev="${current%%-*}"
    local new_rev="${new%%-*}"

    local gw_ini="$G_SCRIPT_PATH/gateway-override.ini"
    if [ -f "$gw_ini" ]; then
        cp "$gw_ini" "$gw_ini.bak"
        sed -i -E "s|^(revision[[:space:]]*=[[:space:]]*)${current_rev}\$|\\1${new_rev}|" "$gw_ini"
        echo "${GREEN}gateway-override.ini revision: ${current_rev} → ${new_rev}${NC}"
    fi

    local world_yaml="$G_SCRIPT_PATH/orchestrator/world.yaml"
    if [ -f "$world_yaml" ]; then
        cp "$world_yaml" "$world_yaml.bak"
        sed -i -E "s|(funcom/self-hosting/[a-z-]+):${current}|\\1:${new}|g" "$world_yaml"
        local cnt
        cnt=$(grep -c "${new}" "$world_yaml" || echo 0)
        echo "${GREEN}orchestrator/world.yaml: ${cnt} tag(s) bumped to ${new}${NC}"
    fi
}

restart_stack() {
    echo "Restarting stack..."
    dc up -d
}

# Promote the files we actually need from .updates/ into server/ and delete
# the rest. Run after verify succeeds. Wrapped in `|| true` so cleanup failures
# don't poison a successful apply.
cleanup_after_apply() {
    if [ "${NO_CLEAN:-}" = "1" ]; then
        echo "Skipping cleanup (--no-clean / NO_CLEAN=1)"
        return
    fi
    echo "Cleaning up..."

    # 1) Promote metadata + the battlegroup tarballs (the latter are insurance
    #    so an operator can re-`docker load` without re-running steamcmd).
    mkdir -p "$SERVER_PATH/images/battlegroup" \
             "$SERVER_PATH/images/operators" \
             "$SERVER_PATH/images/prerequisites" \
             "$SERVER_PATH/scripts/setup/templates"

    cp -f "$STAGING_PATH/images/battlegroup/version.txt" \
          "$SERVER_PATH/images/battlegroup/version.txt" 2>/dev/null || true
    cp -f "$STAGING_PATH/images/operators/version.txt" \
          "$SERVER_PATH/images/operators/version.txt" 2>/dev/null || true
    cp -rf "$STAGING_PATH/images/battlegroup/"*.tar \
          "$SERVER_PATH/images/battlegroup/" 2>/dev/null || true
    cp -f "$STAGING_PATH/images/prerequisites/igw-postgres.tar" \
          "$SERVER_PATH/images/prerequisites/igw-postgres.tar" 2>/dev/null || true
    cp -rf "$STAGING_PATH/scripts/setup/templates/"* \
          "$SERVER_PATH/scripts/setup/templates/" 2>/dev/null || true

    # 2) Nuke staging entirely — we have what we need in server/.
    rm -rf "$STAGING_PATH" 2>/dev/null || true

    # 3) Prune unused stuff in server/ that's been around since the first
    #    install (or got carried over from a prior promote).
    #    Operators: never `docker load`ed — our orchestrator emulates them.
    find "$SERVER_PATH/images/operators" -maxdepth 1 -name '*.tar' -delete 2>/dev/null || true
    rm -rf "$SERVER_PATH/images/operators/crds" 2>/dev/null || true
    #    Prerequisites: only igw-postgres.tar is used; the rest are k3s plumbing.
    find "$SERVER_PATH/images/prerequisites" -maxdepth 1 -name '*.tar' \
        -not -name 'igw-postgres.tar' -delete 2>/dev/null || true
    #    Funcom installer scripts: keep only the templates/ dir setup.sh uses.
    if [ -d "$SERVER_PATH/scripts" ]; then
        find "$SERVER_PATH/scripts" -mindepth 1 -maxdepth 1 \
            -not -name 'setup' -exec rm -rf {} + 2>/dev/null || true
        if [ -d "$SERVER_PATH/scripts/setup" ]; then
            find "$SERVER_PATH/scripts/setup" -mindepth 1 -maxdepth 1 \
                -not -name 'templates' -exec rm -rf {} + 2>/dev/null || true
        fi
    fi
    #    Steam metadata: not needed at runtime.
    rm -rf "$SERVER_PATH/steamapps" 2>/dev/null || true

    # 4) Drop the .bak files left by the apply step. Leaving them would
    #    confuse a future rollback after a *subsequent* apply succeeded —
    #    rollback prefers .bak when present and would revert to the wrong
    #    version. git history / steamapps / the previous tag in compose
    #    are enough to reconstruct if needed.
    rm -f "$COMPOSE_FILE.bak" 2>/dev/null || true
    rm -f "$G_SCRIPT_PATH/gateway-override.ini.bak" 2>/dev/null || true
    rm -f "$G_SCRIPT_PATH/orchestrator/world.yaml.bak" 2>/dev/null || true

    local size
    size=$(du -sh "$SERVER_PATH" 2>/dev/null | awk '{print $1}')
    echo "${GREEN}Cleanup done (server/: $size)${NC}"
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

    # Parse flags. Currently only --no-clean.
    while [ $# -gt 0 ]; do
        case "$1" in
            --no-clean) NO_CLEAN=1; shift ;;
            *) echo "Unknown flag: $1" >&2; exit 1 ;;
        esac
    done

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
        cleanup_after_apply
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

    # Same treatment for the two version-baked sidecars patched on apply.
    local gw_ini="$G_SCRIPT_PATH/gateway-override.ini"
    if [ -f "$gw_ini.bak" ]; then
        mv "$gw_ini.bak" "$gw_ini"
        echo "${GREEN}Restored $gw_ini from .bak${NC}"
    elif [ -f "$gw_ini" ]; then
        local old_rev="${OLD_TAG%%-*}" new_rev="${NEW_TAG%%-*}"
        sed -i -E "s|^(revision[[:space:]]*=[[:space:]]*)${new_rev}\$|\\1${old_rev}|" "$gw_ini"
        echo "${GREEN}Reverted gateway-override.ini revision via sed${NC}"
    fi
    local world_yaml="$G_SCRIPT_PATH/orchestrator/world.yaml"
    if [ -f "$world_yaml.bak" ]; then
        mv "$world_yaml.bak" "$world_yaml"
        echo "${GREEN}Restored $world_yaml from .bak${NC}"
    elif [ -f "$world_yaml" ]; then
        sed -i -E "s|(funcom/self-hosting/[a-z-]+):${NEW_TAG}|\\1:${OLD_TAG}|g" "$world_yaml"
        echo "${GREEN}Reverted orchestrator/world.yaml via sed${NC}"
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
        apply)    shift; cmd_apply "$@" ;;
        verify)   shift; cmd_verify "$@" ;;
        rollback) cmd_rollback ;;
        ""|-h|--help|help)
            cat <<EOF
Usage: $0 {check|apply|verify|rollback}

  check               Download latest from Steam, report version delta. No changes.
  apply [--no-clean]  check + pre-update DB backup + docker load + compose tag swap +
                      restart + verify. Pauses with rollback prompt on verify failure.
                      On success, prunes unused tarballs / Funcom installer scripts /
                      Steam metadata from server/ (pass --no-clean to keep them).
  verify              Re-run the post-update health checks (useful when boots are slow).
  rollback            Revert to the previous tag and restore the pre-update DB snapshot.
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
