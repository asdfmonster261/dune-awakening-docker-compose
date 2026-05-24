#!/bin/bash
# Generate self-signed CA + rabbitmq-game server cert.
# Funcom's services connect with verify_none, so this single cert is enough.
set -euo pipefail

G_SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CERT_DIR="$G_SCRIPT_PATH/certs"

mkdir -p "$CERT_DIR"

need_rabbitmq_cert=true
need_orchestrator_cert=true
if [ -f "$CERT_DIR/key.pem" ] && [ -f "$CERT_DIR/cert.pem" ] && [ -f "$CERT_DIR/cacert.pem" ]; then
    need_rabbitmq_cert=false
fi
if [ -f "$CERT_DIR/orchestrator-key.pem" ] && [ -f "$CERT_DIR/orchestrator-cert.pem" ]; then
    need_orchestrator_cert=false
fi

if ! $need_rabbitmq_cert && ! $need_orchestrator_cert; then
    echo "All certs already present in $CERT_DIR; skipping generation."
    exit 0
fi

# Make sure the CA exists (needed to sign any new certs).
if [ ! -f "$CERT_DIR/cacert.pem" ] || [ ! -f "$CERT_DIR/ca-key.pem" ]; then
    echo "Generating self-signed CA..."
    openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 -nodes \
        -keyout "$CERT_DIR/ca-key.pem" \
        -out "$CERT_DIR/cacert.pem" \
        -subj "/CN=dune-self-host-ca" >/dev/null 2>&1
fi

if $need_rabbitmq_cert; then
    echo "Generating rabbitmq-game server cert..."
    openssl req -newkey rsa:4096 -nodes \
        -keyout "$CERT_DIR/key.pem" \
        -out "$CERT_DIR/server.csr" \
        -subj "/CN=rabbitmq-game" >/dev/null 2>&1

    cat > "$CERT_DIR/server.ext" <<EOF
subjectAltName = DNS:rabbitmq-game, DNS:rabbitmq-admin, DNS:localhost, IP:127.0.0.1
extendedKeyUsage = serverAuth, clientAuth
EOF

    openssl x509 -req \
        -in "$CERT_DIR/server.csr" \
        -CA "$CERT_DIR/cacert.pem" \
        -CAkey "$CERT_DIR/ca-key.pem" \
        -CAcreateserial \
        -out "$CERT_DIR/cert.pem" \
        -days 3650 -sha256 \
        -extfile "$CERT_DIR/server.ext" >/dev/null 2>&1
    rm -f "$CERT_DIR/server.csr" "$CERT_DIR/server.ext"
fi

if $need_orchestrator_cert; then
    echo "Generating dune-orchestrator server cert..."
    openssl req -newkey rsa:4096 -nodes \
        -keyout "$CERT_DIR/orchestrator-key.pem" \
        -out "$CERT_DIR/orchestrator.csr" \
        -subj "/CN=dune-orchestrator" >/dev/null 2>&1

    cat > "$CERT_DIR/orchestrator.ext" <<EOF
subjectAltName = DNS:dune-orchestrator, DNS:kubernetes, DNS:kubernetes.default, DNS:kubernetes.default.svc, DNS:localhost, IP:127.0.0.1
extendedKeyUsage = serverAuth
EOF

    openssl x509 -req \
        -in "$CERT_DIR/orchestrator.csr" \
        -CA "$CERT_DIR/cacert.pem" \
        -CAkey "$CERT_DIR/ca-key.pem" \
        -CAcreateserial \
        -out "$CERT_DIR/orchestrator-cert.pem" \
        -days 3650 -sha256 \
        -extfile "$CERT_DIR/orchestrator.ext" >/dev/null 2>&1
    rm -f "$CERT_DIR/orchestrator.csr" "$CERT_DIR/orchestrator.ext"
fi

rm -f "$CERT_DIR/cacert.srl"
chmod 644 "$CERT_DIR"/*.pem
echo "Certs ready in $CERT_DIR"
