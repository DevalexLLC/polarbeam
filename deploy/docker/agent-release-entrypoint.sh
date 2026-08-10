#!/bin/sh
# Release-image entrypoint. Gates `run` behind a selfcheck preflight so a
# misconfigured or capability-starved container fails loudly at start
# instead of degrading silently (the same guarantee the retired systemd
# unit provided via ExecStartPre). Every other subcommand — enroll,
# selfcheck, version — passes straight through.
set -eu
if [ "${1:-}" = "run" ]; then
    shift
    polarbeam-agent selfcheck "$@"
    exec polarbeam-agent run "$@"
fi
exec polarbeam-agent "$@"
