#!/bin/bash
# Interactive setup: gather inputs, generate secrets/certs, load images, render configs.
# Writes .env at the repo root for docker-compose to consume.
#
# Safe to re-run: when .env already exists, defaults to "update mode" which
# preserves the load-bearing secrets (passwords, FLS token, battlegroup ID)
# and only re-renders the derived config files. A full reset is opt-in and
# warns about data loss.
#
# pipefail is intentionally omitted: the `tr -dc … </dev/urandom | head -c N`
# pattern in generate_secrets SIGPIPEs the upstream tr, which under pipefail
# returns 141 and `set -e` would kill the script silently right after the
# assignment.
set -eu

G_SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$G_SCRIPT_PATH/.." && pwd)"
ENV_FILE="$REPO_ROOT/.env"

RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; NC=$'\033[0m'

# MODE is set by choose_mode: "fresh" (no .env yet), "update" (preserve secrets),
# or "reset" (regenerate everything — data loss warning was acknowledged).
MODE="fresh"

require_cmd() {
    for c in "$@"; do
        if ! command -v "$c" >/dev/null 2>&1; then
            echo "${RED}Missing required command: $c${NC}" >&2
            exit 1
        fi
    done
}

# ── Existing-install detection ──────────────────────────────────────────────

load_existing_env() {
    # Source the existing .env file into the current shell. Values are
    # double-quoted on write, so word-splitting in space-containing values
    # (e.g. WORLD_REGION="North America") is preserved.
    # shellcheck disable=SC1090
    . "$ENV_FILE"

    # Defaults for fields that may be missing from older .env files.
    BACKUP_INTERVAL_SECONDS="${BACKUP_INTERVAL_SECONDS:-3600}"
    BACKUP_RETENTION="${BACKUP_RETENTION:-24}"

    # Reconstruct FLS_PLAYER_ID from the existing WORLD_UNIQUE_NAME.
    # Name format: sh-<player-id>-<6 random lowercase>.
    if [ -n "${WORLD_UNIQUE_NAME:-}" ]; then
        FLS_PLAYER_ID="${WORLD_UNIQUE_NAME#sh-}"
        FLS_PLAYER_ID="${FLS_PLAYER_ID%-*}"
    fi
}

choose_mode() {
    echo
    echo "${YELLOW}.env already exists at $ENV_FILE${NC}"
    echo
    echo "  [u] Update — preserve secrets (DB passwords, FLS token, battlegroup"
    echo "       ID, RMQ secret); re-prompt only world name / display name /"
    echo "       host IP / password. Re-renders derived config files."
    echo
    echo "  [r] ${RED}Full reset${NC} — regenerate all secrets. This will make the"
    echo "       existing postgres data unreadable (passwords change) and"
    echo "       re-register the battlegroup with FLS as a new server."
    echo "       Existing characters will be lost."
    echo
    echo "  [a] Abort"
    echo
    while true; do
        read -r -p "Choice [u/r/a]: " choice
        case "$choice" in
            ""|[uU]*)
                MODE="update"
                return
                ;;
            [rR]*)
                echo
                read -r -p "${RED}Confirm full reset? Type 'reset' to proceed: ${NC}" confirm
                if [ "$confirm" = "reset" ]; then
                    MODE="reset"
                    return
                fi
                echo "Not confirmed; try again."
                ;;
            [aA]*)
                echo "Aborted."
                exit 0
                ;;
            *)
                echo "Invalid choice."
                ;;
        esac
    done
}

# ── Prompts ─────────────────────────────────────────────────────────────────

prompt_world_name() {
    local default="${WORLD_NAME:-}"
    local prompt
    if [ -n "$default" ]; then
        prompt="World name (1-50 chars) [$default]: "
    else
        prompt="World name shown in the server browser (1-50 chars): "
    fi
    while true; do
        read -r -p "$prompt" input
        if [ -z "$input" ] && [ -n "$default" ]; then
            WORLD_NAME="$default"
            return
        fi
        if [ -n "$input" ] && [ "${#input}" -le 50 ]; then
            WORLD_NAME="$input"
            return
        fi
        echo "${RED}Invalid name; must be 1–50 characters.${NC}"
    done
}

prompt_world_region() {
    local regions=("Asia" "Europe" "North America" "Oceania" "South America")
    echo "Select FLS region:"
    select region in "${regions[@]}"; do
        if [ -n "${region:-}" ]; then
            WORLD_REGION="$region"
            return
        fi
        echo "${RED}Invalid selection.${NC}"
    done
}

prompt_fls_token() {
    while true; do
        read -r -p "Self-hosted FLS token (JWT from your dune account page): " FLS_TOKEN
        local payload
        payload=$(printf '%s' "$FLS_TOKEN" | jq -R 'split(".") | .[0:2] | map(@base64d) | map(fromjson)' 2>/dev/null || true)
        if [ -n "$payload" ]; then
            FLS_PLAYER_ID=$(printf '%s' "$payload" | jq -r '.[1].HostId' 2>/dev/null | tr '[:upper:]' '[:lower:]')
            if [ -n "$FLS_PLAYER_ID" ] && [ "$FLS_PLAYER_ID" != "null" ]; then
                return
            fi
        fi
        echo "${RED}Token doesn't look like a valid self-hosting JWT (missing HostId).${NC}"
    done
}

prompt_fls_api_key() {
    read -r -p "FLS API key UUID (press enter to use placeholder for testing): " FLS_API_KEY
    if [ -z "$FLS_API_KEY" ]; then
        FLS_API_KEY="00000000-0000-0000-0000-000000000000"
    fi
}

prompt_host_ip() {
    local default_ip="${HOST_IP:-}"
    if [ -z "$default_ip" ]; then
        default_ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')
        [ -z "$default_ip" ] && default_ip="127.0.0.1"
    fi
    read -r -p "Public/LAN IP players will connect to [$default_ip]: " input
    HOST_IP="${input:-$default_ip}"
}

prompt_display_name() {
    local default="${BROWSER_DISPLAY_NAME:-$WORLD_NAME}"
    read -r -p "Browser display name [$default]: " input
    BROWSER_DISPLAY_NAME="${input:-$default}"
}

prompt_browser_password() {
    # Not stored in .env, so we have no default to show. Empty input leaves
    # whatever is currently in UserEngine.ini untouched (apply_browser_display_name
    # only patches when the variable is non-empty).
    read -r -p "Optional server password (press enter to keep current/none): " BROWSER_PASSWORD
}

# ── Secret + identity generation ────────────────────────────────────────────

generate_secrets() {
    # Only runs in fresh + reset modes. Update mode preserves these from .env.
    WORLD_UNIQUE_NAME="sh-${FLS_PLAYER_ID}-$(tr -dc 'a-z' </dev/urandom | head -c 6)"
    HOST_DATACENTER_ID="dune-${WORLD_REGION// /_}"
    POSTGRES_SUPER_PASS=$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)
    POSTGRES_DUNE_PASS=$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)
    RMQ_HTTP_TOKEN_AUTH_SECRET=$(openssl rand 64 | base64 -w0)
}

# ── File rendering ──────────────────────────────────────────────────────────

write_env_file() {
    # All values are double-quoted so that `source .env` works (a value like
    # WORLD_REGION="North America" would otherwise split on the space and run
    # `America` as a command). Docker compose's .env parser strips the quotes
    # on read, so interpolation in docker-compose.yml is unaffected.
    cat > "$ENV_FILE" <<EOF
# Generated by dune-setup/setup.sh on $(date -Iseconds)
# Edit by re-running setup.sh; manual edits won't be reflected in derived files
# (gateway-override.ini, UserEngine.ini) unless you re-run.

WORLD_NAME="${WORLD_NAME}"
WORLD_REGION="${WORLD_REGION}"
WORLD_UNIQUE_NAME="${WORLD_UNIQUE_NAME}"
HOST_IP="${HOST_IP}"
HOST_DATACENTER_ID="${HOST_DATACENTER_ID}"

FLS_TOKEN="${FLS_TOKEN}"
FLS_API_KEY="${FLS_API_KEY}"

POSTGRES_SUPER_PASS="${POSTGRES_SUPER_PASS}"
POSTGRES_DUNE_PASS="${POSTGRES_DUNE_PASS}"
RMQ_HTTP_TOKEN_AUTH_SECRET="${RMQ_HTTP_TOKEN_AUTH_SECRET}"

BROWSER_DISPLAY_NAME="${BROWSER_DISPLAY_NAME}"

# postgres-backup sidecar — adjust to taste. The defaults give one snapshot
# per hour with a 24-hour rolling window. After changing, re-create just
# the sidecar so it picks up the new values:
#   docker compose up -d --force-recreate postgres-backup
BACKUP_INTERVAL_SECONDS="${BACKUP_INTERVAL_SECONDS}"
BACKUP_RETENTION="${BACKUP_RETENTION}"
EOF
    chmod 600 "$ENV_FILE"
    echo "${GREEN}Wrote $ENV_FILE${NC}"
}

apply_browser_display_name() {
    # Uncomment + set the Bgd.ServerDisplayName line in the shared UserEngine.ini.
    # Game-servers re-read this on next restart.
    local ini="$G_SCRIPT_PATH/usersettings/UserEngine.ini"
    local escaped="${BROWSER_DISPLAY_NAME//\"/}"
    sed -i -E "s|^;?Bgd\\.ServerDisplayName=.*$|Bgd.ServerDisplayName=\"${escaped}\"|" "$ini"

    if [ -n "${BROWSER_PASSWORD:-}" ]; then
        local pw_escaped="${BROWSER_PASSWORD//\"/}"
        sed -i -E "s|^;?Bgd\\.ServerLoginPassword=.*$|Bgd.ServerLoginPassword=\"${pw_escaped}\"|" "$ini"
    fi
}

render_gateway_override() {
    local src="$G_SCRIPT_PATH/gateway-override.ini.template"
    local dst="$G_SCRIPT_PATH/gateway-override.ini"
    sed \
        -e "s|{WORLD_UNIQUE_NAME}|${WORLD_UNIQUE_NAME}|g" \
        -e "s|{WORLD_NAME}|${WORLD_NAME}|g" \
        -e "s|{WORLD_REGION}|${WORLD_REGION}|g" \
        -e "s|{FLS_TOKEN}|${FLS_TOKEN}|g" \
        -e "s|{FLS_API_KEY}|${FLS_API_KEY}|g" \
        -e "s|{HOST_DATACENTER_ID}|${HOST_DATACENTER_ID}|g" \
        -e "s|{HOST_IP}|${HOST_IP}|g" \
        -e "s|{POSTGRES_DUNE_PASS}|${POSTGRES_DUNE_PASS}|g" \
        -e "s|{RMQ_HTTP_TOKEN_AUTH_SECRET}|${RMQ_HTTP_TOKEN_AUTH_SECRET}|g" \
        "$src" > "$dst"
    chmod 600 "$dst"
    echo "${GREEN}Rendered $dst${NC}"
}

generate_service_account() {
    # bg-director + game-servers expect a Kubernetes ServiceAccount mount at
    # /var/run/secrets/kubernetes.io/serviceaccount/{token,ca.crt,namespace}.
    # The orchestrator doesn't validate the token, but the K8s client libraries
    # need the files to exist and the CA to sign the orchestrator's TLS cert.
    local sa_dir="$G_SCRIPT_PATH/orchestrator/serviceaccount"
    if [ "$MODE" = "update" ] && [ -d "$sa_dir" ] && [ -f "$sa_dir/token" ]; then
        # Update namespace in case WORLD_UNIQUE_NAME changed (shouldn't in
        # update mode, but cheap to keep in sync).
        printf 'funcom-seabass-%s' "$WORLD_UNIQUE_NAME" > "$sa_dir/namespace"
        echo "${GREEN}Service-account already exists; preserved token${NC}"
        return
    fi
    mkdir -p "$sa_dir"
    openssl rand -base64 48 | tr -d '\n' > "$sa_dir/token"
    printf 'funcom-seabass-%s' "$WORLD_UNIQUE_NAME" > "$sa_dir/namespace"
    cp "$G_SCRIPT_PATH/certs/cacert.pem" "$sa_dir/ca.crt"
    chmod 644 "$sa_dir"/*
    echo "${GREEN}Wrote service-account at $sa_dir${NC}"
}

render_orchestrator_world() {
    # The orchestrator serves the BattleGroup CR via GET. Render the K8s
    # world-template.yaml with our env values so the orchestrator has the same
    # data the K8s setup would have written to the cluster.
    local src="$REPO_ROOT/server/scripts/setup/templates/world-template.yaml"
    local dst="$G_SCRIPT_PATH/orchestrator/world.yaml"
    if [ ! -f "$src" ]; then
        echo "${YELLOW}WARNING: $src not found; orchestrator will start with empty CR${NC}"
        return
    fi
    sed \
        -e "s|{WORLD_NAME}|${WORLD_NAME}|g" \
        -e "s|{WORLD_UNIQUE_NAME}|${WORLD_UNIQUE_NAME}|g" \
        -e "s|{WORLD_REGION}|${WORLD_REGION}|g" \
        -e "s|{WORLD_IMAGE_TAG}|1968181-0-shipping|g" \
        -e "s|{WORLD_POSTGRES_PASS}|${POSTGRES_SUPER_PASS}|g" \
        -e "s|{WORLD_DUNE_PASS}|${POSTGRES_DUNE_PASS}|g" \
        -e "s|{FLS_SECRET}|${FLS_TOKEN}|g" \
        -e "s|{RMQ_SECRET}|${RMQ_HTTP_TOKEN_AUTH_SECRET}|g" \
        "$src" > "$dst"

    # Funcom's run.sh in the game-server image collapses argv to a string via
    # `echo "$@"` and re-parses through `su -c`. Any cmdline arg with a space
    # gets word-split there — `-FarmRegion=North America` becomes two args,
    # the map URL load fails, and the server requests exit during init.
    # Wrap the value in double quotes so the inner shell preserves it.
    sed -i -E 's|(- -FarmRegion=)([^"][^"]*[^"])$|\1"\2"|' "$dst"
    chmod 644 "$dst"
    echo "${GREEN}Rendered $dst${NC}"
}

# ── Mode-aware orchestration ────────────────────────────────────────────────

run_fresh_or_reset_prompts() {
    prompt_world_name
    prompt_world_region
    prompt_fls_token
    prompt_fls_api_key
    prompt_host_ip
    prompt_display_name
    prompt_browser_password
    BACKUP_INTERVAL_SECONDS="${BACKUP_INTERVAL_SECONDS:-3600}"
    BACKUP_RETENTION="${BACKUP_RETENTION:-24}"
}

run_update_prompts() {
    # Only the safe-to-change fields. Everything else stays exactly as it
    # was loaded from .env.
    prompt_world_name
    prompt_host_ip
    prompt_display_name
    prompt_browser_password
}

maybe_generate_certs() {
    local certs_dir="$G_SCRIPT_PATH/certs"
    if [ "$MODE" = "update" ] && [ -d "$certs_dir" ] && [ -f "$certs_dir/cacert.pem" ]; then
        echo "${GREEN}Certs already exist; skipping regeneration${NC}"
        return
    fi
    "$G_SCRIPT_PATH/generate-certs.sh"
}

main() {
    require_cmd jq openssl docker ip
    echo "${GREEN}Dune Awakening — Docker Compose setup${NC}"

    if [ -f "$ENV_FILE" ]; then
        choose_mode
        load_existing_env
    fi

    case "$MODE" in
        fresh|reset)
            run_fresh_or_reset_prompts
            generate_secrets
            ;;
        update)
            run_update_prompts
            # Secrets preserved from load_existing_env.
            ;;
    esac

    write_env_file
    apply_browser_display_name
    render_gateway_override

    maybe_generate_certs
    generate_service_account
    render_orchestrator_world
    "$G_SCRIPT_PATH/load-images.sh"

    echo
    echo "${GREEN}Setup complete (mode: $MODE).${NC}"
    echo "Next:"
    if [ "$MODE" = "update" ]; then
        echo "  docker compose up -d --force-recreate                   # apply config changes"
    else
        echo "  docker compose up -d                                    # start the always-on stack"
        echo "  docker compose --profile on-demand create               # materialize the 28 on-demand maps (stopped)"
    fi
    echo "  docker compose logs -f                                  # tail logs"
    echo "  docker compose down                                     # stop"
}

main "$@"
