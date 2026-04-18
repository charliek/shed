#!/bin/bash
set -e

# Reload systemd to pick up the unit file
systemctl daemon-reload

# Enable the service (but do NOT start it)
systemctl enable shed-server.service || true

echo ""
echo "shed-server has been installed and enabled."
echo ""
echo "Next steps:"
echo "  1. Edit /etc/shed/server.yaml"
echo "  2. Run: sudo shed-server setup"
echo "  3. Run: sudo systemctl start shed-server"
