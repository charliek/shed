"""The hermetic broker's guards (`broker_env.py`): the pre-spawn check refuses an
env or a broker file that could reach outside the harness, and the post-launch
check refuses a running broker that serves anything but the launch's mocks.

The unit cells launch nothing; the last cell launches one app on purpose with a
stray loopback listener in its broker's server file. None of them touches the
developer's own agent or config: every path is under a throwaway dir, and the
sentinels are listeners these cells start.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import tempfile
import threading
from pathlib import Path

import pytest

import broker_env
import ui
from client import TauriClient
from mockserver import MockShedServer

FIXTURES = Path(__file__).resolve().parent / "fixtures"
MOCK_URL = "http://127.0.0.1:43123"


class _Sentinel:
    """A listener that records every connection and answers nothing."""

    def __init__(self, family: int, address):
        self.sock = socket.socket(family, socket.SOCK_STREAM)
        self.sock.bind(address)
        self.sock.listen(8)
        self.sock.settimeout(0.2)
        self.connections = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._accept, daemon=True)
        self._thread.start()

    def _accept(self) -> None:
        while not self._stop.is_set():
            try:
                conn, _ = self.sock.accept()
            except (socket.timeout, OSError):
                continue
            self.connections += 1
            conn.close()

    def close(self) -> None:
        self._stop.set()
        self._thread.join(timeout=2)
        self.sock.close()


@pytest.fixture
def runtime_dir():
    # Short prefix: the app's socket lives under it (SUN_LEN on macOS).
    d = Path(tempfile.mkdtemp(prefix="shed-be-"))
    try:
        yield d
    finally:
        shutil.rmtree(d, ignore_errors=True)


def _launch_env(runtime_dir: Path, broker) -> dict[str, str]:
    """This process's env with the broker's overrides applied (what `ui.subproc_env`
    hands the check), so each cell can break one thing in it."""
    return broker_env.apply(dict(os.environ), broker[0])


def test_hermetic_env_validates(runtime_dir):
    # The positive control the negative cells below are measured against.
    broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": MOCK_URL})
    broker_env.validate(_launch_env(runtime_dir, broker), broker[1])


def test_validate_refuses_an_ssh_agent_socket(runtime_dir):
    broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": MOCK_URL})
    env = _launch_env(runtime_dir, broker)
    sentinel = _Sentinel(socket.AF_UNIX, str(runtime_dir / "agent.sock"))
    try:
        env["SSH_AUTH_SOCK"] = str(runtime_dir / "agent.sock")
        with pytest.raises(broker_env.HermeticityError, match="SSH_AUTH_SOCK"):
            broker_env.validate(env, broker[1])
        assert sentinel.connections == 0
    finally:
        sentinel.close()


def test_validate_refuses_a_non_loopback_server(runtime_dir):
    broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": MOCK_URL})
    env = _launch_env(runtime_dir, broker)
    info = broker[1]
    info.discovery_path.write_text(
        info.discovery_path.read_text()
        + "    elsewhere:\n        api_url: http://198.51.100.1:8080\n"
        + "        host: 198.51.100.1\n")
    with pytest.raises(broker_env.HermeticityError, match="not the server file"):
        broker_env.validate(env, info)
    # Naming it up front is refused too.
    with pytest.raises(broker_env.HermeticityError, match="198.51.100.1"):
        broker_env.write_hermetic_broker(runtime_dir, {"elsewhere": "http://198.51.100.1:8080"})


def test_validate_refuses_a_flow_style_server_file(runtime_dir):
    # Valid YAML a line scanner reads as "no servers"; the broker reads a server.
    broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": MOCK_URL})
    env = _launch_env(runtime_dir, broker)
    broker[1].discovery_path.write_text(
        "servers: {outside: {host: 198.51.100.1, http_port: 8080}}\n")
    with pytest.raises(broker_env.HermeticityError, match="not the server file"):
        broker_env.validate(env, broker[1])


def test_validate_refuses_a_url_the_two_parsers_read_differently(runtime_dir):
    # Python's URL parser reads this as 127.0.0.1:43123 (one of the mocks); the
    # broker's reads the backslash as the end of the authority: 198.51.100.1:80.
    tricky = "http://198.51.100.1\\@127.0.0.1:43123"
    broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": MOCK_URL})
    env = _launch_env(runtime_dir, broker)
    broker[1].discovery_path.write_text(
        "servers:\n    mini3:\n        host: 198.51.100.1\n"
        f"        api_url: {tricky}\n")
    with pytest.raises(broker_env.HermeticityError, match="not the server file"):
        broker_env.validate(env, broker[1])
    with pytest.raises(broker_env.HermeticityError, match="only a mock"):
        broker_env.write_hermetic_broker(runtime_dir, {"mini3": tricky})


def test_validate_refuses_a_proxy(runtime_dir):
    broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": MOCK_URL})
    env = _launch_env(runtime_dir, broker)
    assert env["NO_PROXY"] == "*" and env["no_proxy"] == "*"
    env["HTTPS_PROXY"] = "http://198.51.100.2:3128"
    with pytest.raises(broker_env.HermeticityError, match="HTTPS_PROXY"):
        broker_env.validate(env, broker[1])


def test_validate_refuses_a_state_dir_outside(runtime_dir):
    broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": MOCK_URL})
    env = _launch_env(runtime_dir, broker)
    outside = Path(tempfile.mkdtemp(prefix="shed-be-out-"))
    try:
        env["SHED_BROKER_STATE_DIR"] = str(outside)
        with pytest.raises(broker_env.HermeticityError, match="SHED_BROKER_STATE_DIR"):
            broker_env.validate(env, broker[1])
    finally:
        shutil.rmtree(outside, ignore_errors=True)


def test_post_launch_check_refuses_a_stray_listener(runtime_dir):
    # The pre-spawn check passes; the broker's server file is then given a second
    # loopback listener that is not one of the launch's mocks (a sentinel that
    # records connections), as a file changed behind the check would be. The
    # broker dials it, and the post-launch check must refuse the run.
    mock = MockShedServer()
    mock.start()
    sentinel = _Sentinel(socket.AF_INET, ("127.0.0.1", 0))
    proc = log_fh = None
    try:
        cfg = ui._SUBPROC["tauri"]
        broker = broker_env.write_hermetic_broker(runtime_dir, {"mini3": mock.base_url})
        env = ui.subproc_env(cfg, runtime_dir=runtime_dir, mock_base_url=mock.base_url,
                             config_path=FIXTURES / "config.yaml", broker=broker)
        port = sentinel.sock.getsockname()[1]
        stray = f"http://127.0.0.1:{port}"
        info = broker[1]
        info.discovery_path.write_text(
            info.discovery_path.read_text()
            + f"    stray:\n        host: 127.0.0.1\n        http_port: {port}\n")
        log = runtime_dir / "be-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh,
                                stderr=subprocess.STDOUT)
        sock = runtime_dir / cfg.sock_rel
        ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url, proc=proc, log=log)
        client = TauriClient(sock)
        try:
            with pytest.raises(broker_env.HermeticityError, match=stray):
                broker_env.check_running(client, info)
            client.wait_until(lambda: sentinel.connections > 0, timeout=20,
                              what="the broker dialled the stray listener")
        finally:
            client.close()
    finally:
        ui.terminate(proc)
        if log_fh is not None:
            log_fh.close()
        sentinel.close()
        mock.stop()
