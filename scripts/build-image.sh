#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
TAG="${TAG:-shed-base:latest}"

echo "Building shed-base image..."
docker build -t "$TAG" "$REPO_ROOT/images/shed-base"
echo "✓ Built $TAG"
