"""The `version` verb — the cheapest end-to-end proof that both binaries built,
run, and agree on the SHAPE of their identity line (plan 009 §3.6: the version
surface is diffed masked, never by value)."""

from normalize import mask_version


def test_version_line_shape(differential, isolated):
    def scenario(impl):
        leg = isolated(impl)
        res = leg.run("version")
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        assert res.stderr == "", f"{impl}: version wrote to stderr: {res.stderr!r}"
        return {"code": res.returncode, "stdout": mask_version(res.stdout)}

    differential(scenario)
