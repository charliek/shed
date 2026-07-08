"""M0 ship-gates: the deferred Phase-1 safety nets the Rust-core default flip requires.

Run against a built `build/ShedDesktop.app` (release for the real gate). Four checks,
all of which must pass before the Rust core ships as the macOS default:

  1. arch      — the shipped Mach-Os are arm64-only (no x86_64 slice). The core is
                 built arm64-only (scripts/build-core.sh), so a universal app would be a
                 default-on trap: an Intel slice would have no Rust core to link.
  2. size      — the app binary is within a byte budget (a Rust-core size blowup fails).
  3. cold-launch — time-to-`identify` is within budget on each backend (a hang in Rust
                 init fails, not just a slow test).
  4. golden    — backend-sensitive IPC payloads are BYTE-IDENTICAL across the Rust and
                 Swift backends against the same hermetic mock. Both backends serialize
                 the IPC envelope through the same Swift `Encodable`, so any diff is a
                 real decode/mapping divergence (the cross-backend parity guard Phase 1
                 deferred). See plans/phase-2-rust-clients.md M0.

Standalone (NOT a pytest module): it owns the app lifecycle and launches it twice
(rust, then swift via SHED_DESKTOP_RUST_CORE=0). Exits non-zero on any failure.

Budgets are env-overridable:
  SHED_DESKTOP_SIZE_BUDGET_MB        (default 60)
  SHED_DESKTOP_COLD_LAUNCH_BUDGET_S  (default 15)
"""

from __future__ import annotations

import difflib
import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import ui
from client import ShedDesktop

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"

# Backend-sensitive read ops: each flows host data through the active backend
# (Rust adapter or Swift URLSession) and back out as an IPC payload. host.list is
# a config-derived control that must be identical regardless of backend.
SIZE_BUDGET_MB = float(os.environ.get("SHED_DESKTOP_SIZE_BUDGET_MB", "60"))
COLD_LAUNCH_BUDGET_S = float(os.environ.get("SHED_DESKTOP_COLD_LAUNCH_BUDGET_S", "15"))


class GateFailure(Exception):
    pass


def _macho_archs(path: Path) -> set[str]:
    out = subprocess.run(["lipo", "-archs", str(path)],
                         capture_output=True, text=True)
    if out.returncode != 0:
        raise GateFailure(f"lipo -archs {path} failed: {out.stderr.strip()}")
    return set(out.stdout.split())


def check_arch(app: Path) -> None:
    """The app + embedded shedctl must be arm64-only. If the app ever goes
    universal, ShedBackend.start must arch-gate the default (the arm64-only core
    can't back an x86_64 slice) — fail here so that decision isn't skipped."""
    binaries = [
        app / "Contents/MacOS/ShedDesktop",
        app / "Contents/Resources/bin/shedctl",
    ]
    for b in binaries:
        archs = _macho_archs(b)
        if archs != {"arm64"}:
            raise GateFailure(
                f"{b} is {sorted(archs)}, expected arm64-only. A universal build "
                "needs ShedBackend.start to arch-gate the Rust-core default (or a "
                "lipo'd universal xcframework) — see plans/phase-2-rust-clients.md M0.")
    print(f"  arch: arm64-only  ({', '.join(b.name for b in binaries)})")


def check_size(app: Path) -> None:
    binary = app / "Contents/MacOS/ShedDesktop"
    mb = binary.stat().st_size / (1024 * 1024)
    if mb > SIZE_BUDGET_MB:
        raise GateFailure(
            f"ShedDesktop binary is {mb:.1f} MB, over the {SIZE_BUDGET_MB:.0f} MB budget "
            "(raise SHED_DESKTOP_SIZE_BUDGET_MB if this is an intentional growth).")
    print(f"  size: {mb:.1f} MB  (budget {SIZE_BUDGET_MB:.0f} MB)")


def _capture(c: ShedDesktop) -> dict:
    """Backend-sensitive IPC payloads, each of which exercises the active backend
    (Rust adapter or Swift URLSession)."""
    c.refresh()
    c.wait_until(lambda: len(c.sheds_list()) >= 1, what="sheds populated")
    return {
        "sheds.list": c.sheds_list(),
        "system.df": c.system_df(),
        "images.list": c.images_list(),
    }


def check_payload_success(backend: str, payloads: dict) -> None:
    """Reject golden inputs that are only byte-identical because both sides failed."""
    if not payloads.get("sheds.list"):
        raise GateFailure(f"{backend} sheds.list returned no sheds")
    for op, value_key in (("system.df", "usage"), ("images.list", "images")):
        rows = payloads.get(op) or []
        if not rows:
            raise GateFailure(f"{backend} {op} returned no host rows")
        for row in rows:
            host = row.get("host", "?")
            if row.get("error"):
                raise GateFailure(f"{backend} {op}[{host}] returned error: {row['error']}")
            if row.get(value_key) is None:
                raise GateFailure(f"{backend} {op}[{host}] returned no {value_key} payload")


def run_backend(mock_base_url: str, backend: str) -> tuple[dict, float]:
    """Launch the app on `backend` ('rust'|'swift'), timing the cold launch, and
    capture the backend-sensitive payloads. Leaves the app quit."""
    if backend == "swift":
        os.environ["SHED_DESKTOP_RUST_CORE"] = "0"
    else:
        os.environ.pop("SHED_DESKTOP_RUST_CORE", None)  # unset ⇒ rust (default-on)
    ui.quit()
    state_dir = Path(tempfile.mkdtemp(prefix=f"shed-desktop-m0-{backend}-"))
    t0 = time.monotonic()
    ui.launch(mock_base_url=mock_base_url, config_path=CONFIG, state_dir=state_dir)
    cold = time.monotonic() - t0  # launch() blocks on wait_alive (hermetic + core match)
    c = ShedDesktop(ui.socket_path())
    try:
        payloads = _capture(c)
    finally:
        c.close()
    ui.quit()
    return payloads, cold


def check_cold_launch(backend: str, cold: float) -> None:
    if cold > COLD_LAUNCH_BUDGET_S:
        raise GateFailure(
            f"{backend} cold-launch (to identify) took {cold:.1f}s, over the "
            f"{COLD_LAUNCH_BUDGET_S:.0f}s budget "
            "(raise SHED_DESKTOP_COLD_LAUNCH_BUDGET_S on a slow runner).")
    print(f"  cold-launch [{backend}]: {cold:.1f}s  (budget {COLD_LAUNCH_BUDGET_S:.0f}s)")


def _canon(obj) -> str:
    # sort_keys normalizes the (already-identical) Swift CodingKey order so the
    # diff is purely about values; the two backends feed the same Swift encoder,
    # so a diff here is a genuine decode/mapping divergence.
    return json.dumps(obj, sort_keys=True, ensure_ascii=False, indent=2)


def check_golden(rust: dict, swift: dict) -> None:
    ops = sorted(set(rust) | set(swift))
    mismatches = []
    for op in ops:
        r, s = _canon(rust.get(op)), _canon(swift.get(op))
        if r != s:
            mismatches.append((op, r, s))
    if mismatches:
        for op, r, s in mismatches:
            print(f"\n✗ golden mismatch on {op} (rust vs swift):", file=sys.stderr)
            diff = difflib.unified_diff(
                r.splitlines(), s.splitlines(), fromfile=f"{op}[rust]",
                tofile=f"{op}[swift]", lineterm="")
            print("\n".join(diff), file=sys.stderr)
        raise GateFailure(
            f"{len(mismatches)} backend-sensitive payload(s) differ across the Rust "
            "and Swift backends — the golden cross-backend parity gate.")
    print(f"  golden: {len(ops)} payload(s) byte-identical across rust/swift  "
          f"({', '.join(ops)})")


def main() -> int:
    app = ui.APP
    if not app.is_dir():
        print(f"error: {app} not found — build it first "
              "(`./scripts/bundle.sh release`)", file=sys.stderr)
        return 2

    # Lazy import so the module loads even when the mock's deps aren't on a plain
    # `python m0_ship_gates.py` (the make target runs it under `uv --group test`).
    from mockserver import MockShedServer

    print("== M0 ship-gates ==")
    failures = []

    # Static gates (no launch needed).
    for name, fn in (("arch", check_arch), ("size", check_size)):
        try:
            fn(app)
        except GateFailure as e:
            failures.append(f"{name}: {e}")
            print(f"  ✗ {name}: {e}", file=sys.stderr)

    # Dynamic gates: launch each backend against one shared mock.
    mock = MockShedServer()
    mock.start()
    try:
        results = {}
        for backend in ("rust", "swift"):
            payloads, cold = run_backend(mock.base_url, backend)
            try:
                check_payload_success(backend, payloads)
                results[backend] = payloads
            except GateFailure as e:
                failures.append(f"payload[{backend}]: {e}")
                print(f"  ✗ payload[{backend}]: {e}", file=sys.stderr)
            try:
                check_cold_launch(backend, cold)
            except GateFailure as e:
                failures.append(f"cold-launch[{backend}]: {e}")
                print(f"  ✗ cold-launch[{backend}]: {e}", file=sys.stderr)
        if "rust" in results and "swift" in results:
            try:
                check_golden(results["rust"], results["swift"])
            except GateFailure as e:
                failures.append(f"golden: {e}")
    finally:
        mock.stop()
        ui.quit()

    if failures:
        print(f"\n✗ M0 ship-gates FAILED ({len(failures)}):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1
    print("\n✓ M0 ship-gates PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
