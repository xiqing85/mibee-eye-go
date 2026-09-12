#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
# Use REMOTE_HOST env var or command-line argument
# Usage: ./deploy.sh [user@host]
REMOTE="${1:-${REMOTE_HOST:-user@your-rpi-host}}"
REMOTE_DIR="~/mibee-eye"

echo "Building for arm64..."
cd "$PROJECT_DIR"
make build

echo "Deploying to $REMOTE..."
ssh "$REMOTE" "mkdir -p $REMOTE_DIR"

# Stop mediamtx before deploying to free camera
ssh "$REMOTE" 'sudo systemctl stop mediamtx'
ssh "$REMOTE" 'sudo systemctl disable mediamtx'
scp build/mibee-eye "$REMOTE:$REMOTE_DIR/"
scp configs/config.yaml "$REMOTE:$REMOTE_DIR/"

# Install service
scp deploy/mibee-eye.service "$REMOTE:/tmp/"
ssh "$REMOTE" "sudo mv /tmp/mibee-eye.service /etc/systemd/system/ && sudo systemctl daemon-reload && sudo systemctl enable mibee-eye"

echo "Deploy complete. Run 'make service-restart' to start."
