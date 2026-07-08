"""Post-install check for the shed-desktop .deb: launch the installed binary under a
virtual display and assert it answers `identify` (proving the runtime deps are
satisfied, the WebKitGTK window realizes, and the IPC socket binds). Run inside a
clean container after `apt-get install ./shed-desktop_*.deb xvfb`. No mock needed —
identify is backend-independent (test mode without a mock builds no clients).

The shipped Linux client is the Tauri app, so the WebKitGTK web-process needs the
headless render env below (Xvfb has no GPU) or the content process dies on boot."""

import json
import os
import socket
import subprocess
import sys
import time

SOCK = "/tmp/shed-tauri-debcheck.sock"


def main() -> int:
    env = dict(
        os.environ,
        SHED_TAURI_TEST_MODE="1",
        SHED_TAURI_SOCKET=SOCK,
        XDG_RUNTIME_DIR="/tmp",
        # Headless WebKitGTK: software rendering, no dmabuf/compositing (Xvfb has
        # no GPU) — mirrors Dockerfile.tauri-linux's render env.
        WEBKIT_DISABLE_DMABUF_RENDERER="1",
        WEBKIT_DISABLE_COMPOSITING_MODE="1",
        LIBGL_ALWAYS_SOFTWARE="1",
        GDK_BACKEND="x11",
    )
    proc = subprocess.Popen(["xvfb-run", "-a", "shed-desktop"], env=env)
    try:
        deadline = time.time() + 30
        while time.time() < deadline:
            if proc.poll() is not None:
                print(f"FAIL: shed-desktop exited early ({proc.returncode})", file=sys.stderr)
                return 1
            try:
                s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                s.settimeout(10)
                s.connect(SOCK)
                s.sendall(b'{"id":"1","op":"identify","params":{}}\n')
                buf = b""
                while b"\n" not in buf:
                    chunk = s.recv(65536)
                    if not chunk:
                        break
                    buf += chunk
                s.close()
                r = json.loads(buf.split(b"\n")[0])
                assert r["ok"], r
                info = r["result"]
                assert info["platform"] == "tauri" and info["core"] == "rust", info
                print(f"DEB IDENTIFY OK: platform={info['platform']} core={info['core']}")
                # The bundled CLI must be able to drive the shipped app: run the
                # installed /usr/bin/shedctl against the same socket and assert it
                # answers (proves the .deb didn't ship a broken/mis-targeted shedctl).
                ctl = subprocess.run(
                    ["shedctl", "--socket", SOCK, "identify"],
                    capture_output=True,
                    text=True,
                    timeout=15,
                )
                if ctl.returncode != 0:
                    print(f"FAIL: shedctl identify exited {ctl.returncode}: {ctl.stderr}", file=sys.stderr)
                    return 1
                print(f"DEB SHEDCTL OK: {ctl.stdout.strip()}")
                return 0
            except OSError:
                time.sleep(0.5)
        print("FAIL: shed-desktop never answered identify", file=sys.stderr)
        return 1
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())
