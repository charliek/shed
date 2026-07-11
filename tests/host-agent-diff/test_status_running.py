"""A running daemon's `status --json` (a `LiveStatus` blob) and `status` text render
are equal across the impls after masking the volatile fields (D3 normalization). The
'watch none' config makes BOTH report an empty `servers` list."""

import json

import pytest

from normalize import canonical, mask_live_status, mask_status_text


@pytest.mark.differential
def test_status_json_masked_canonical_equal(daemon, watch_none_config, differential):
    def scenario(impl):
        with daemon(impl, watch_none_config) as d:
            r = d.status(json=True)
            assert r.returncode == 0, (
                f"{impl}: status --json exit {r.returncode}\n{r.stderr}"
            )
            obj = json.loads(r.stdout)
            return canonical(mask_live_status(obj, d.socket_dir, d.config_path))

    masked = differential(scenario)

    # The stable fields survived the mask and carry the expected values.
    assert masked["schema"] == 1
    assert masked["policies"] == {
        "ssh-agent": "shed-desktop",
        "aws-credentials": "approve-all",
        "docker-credentials": "deny-all",
    }
    assert masked["gate_namespaces"] == ["ssh-agent"]
    assert masked["servers"] == []
    assert masked["approval_channel"]["consumer_connected"] is False
    assert masked["approval_channel"]["socket_path"] == "<dir>/host-agent.sock"


@pytest.mark.differential
def test_status_text_masked_equal(daemon, watch_none_config, differential):
    def scenario(impl):
        with daemon(impl, watch_none_config) as d:
            r = d.status(json=False)
            assert r.returncode == 0, f"{impl}: status exit {r.returncode}\n{r.stderr}"
            return mask_status_text(r.stdout, d.socket_dir, d.config_path)

    text = differential(scenario)

    # Landmarks the render must preserve (the ssh gate is annotated; zero servers).
    assert "Approval policies:" in text
    assert "(decided in shed-desktop)" in text
    assert "none connected (shed-desktop-policy requests fail closed)" in text
    assert "Servers (0):" in text
    assert "(none being watched)" in text
    assert "  socket    <dir>/host-agent.sock" in text
