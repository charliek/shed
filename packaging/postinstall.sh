#!/bin/bash
set -e

# Guard against non-systemd environments (chroots, container builds)
if command -v systemctl >/dev/null 2>&1 && systemctl --system 2>/dev/null; then
    systemctl daemon-reload
    systemctl enable shed-server.service || true
fi

echo ""
echo "shed-server has been installed and enabled."
echo ""
echo "Next steps:"
echo "  1. Edit /etc/shed/server.yaml"
echo "  2. Run: sudo shed-server setup"
echo "  3. Run: sudo systemctl start shed-server"
