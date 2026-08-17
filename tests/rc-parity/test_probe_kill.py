"""`probe` / `kill` — the live-state read and the idempotent teardown.

The lifecycle pinned here is one cell on purpose: probe a live session, kill it,
probe again (gone → exit 4), kill again (still exit 0). Splitting it would need
four creates to say the same thing, and the ORDER is what the contract is about.
"""

from normalize import mask_session

SLUG = "cc3333"


def test_probe_reports_a_live_session(differential, isolated):
    """A `shell` session whose pane has drawn is `ready` (its prompt IS the ready
    signal). The pane is polled to a deadline first, so the classification is
    deterministic rather than a race against the shell's first draw."""

    def scenario(impl):
        leg = isolated(impl)
        created = leg.run(
            "create", "--kind", "shell", "--slug", SLUG, "--name", "parity-probe"
        )
        assert created.returncode == 0, f"{impl}: {created.stderr}"
        leg.wait_for_session(f"rc-{SLUG}")
        leg.wait_for_pane(f"rc-{SLUG}")
        probed = leg.run("probe", "--slug", SLUG)
        assert probed.returncode == 0, f"{impl}: exit {probed.returncode}: {probed.stderr}"
        dto = probed.json()
        assert dto["state"] == "ready", dto
        return mask_session(dto, str(leg.home))

    differential(scenario)


def test_kill_is_idempotent_and_probe_then_reports_gone(differential, isolated):
    """`kill` on a MISSING slug is exit 0 — the idempotence pin (a caller
    reconciling state must not have to distinguish "killed it" from "it was
    already gone") — while `probe` and `accept-trust` on one are exit 4."""

    def scenario(impl):
        leg = isolated(impl)
        created = leg.run(
            "create", "--kind", "shell", "--slug", SLUG, "--name", "parity-kill"
        )
        assert created.returncode == 0, f"{impl}: {created.stderr}"
        leg.wait_for_session(f"rc-{SLUG}")

        first_kill = leg.run("kill", "--slug", SLUG)
        second_kill = leg.run("kill", "--slug", SLUG)
        probe_gone = leg.run("probe", "--slug", SLUG)
        trust_gone = leg.run("accept-trust", "--slug", "dd4444")
        assert f"rc-{SLUG}" not in leg.sessions()
        return {
            "kill": [first_kill.returncode, first_kill.stdout, first_kill.stderr],
            "kill_again": [second_kill.returncode, second_kill.stdout, second_kill.stderr],
            "probe_gone": probe_gone.returncode,
            "accept_trust_missing": trust_gone.returncode,
        }

    result = differential(scenario)
    assert result["kill"][0] == 0
    assert result["kill_again"][0] == 0
    assert result["probe_gone"] == 4
    assert result["accept_trust_missing"] == 4
