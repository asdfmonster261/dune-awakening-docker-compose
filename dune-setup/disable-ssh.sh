#!/bin/sh
# Stub mounted at /etc/init.d/ssh inside each game-server container so that
# run.sh's `service ssh start` is a no-op. The SSH wrapper is K8s-pod-specific
# (lets ops shell into a pod) and is harmless to disable for Docker Compose.
exit 0
