#!/bin/sh
set -eu

if [ "${DOCKLATTICE_NETEM:-0}" = 1 ]; then
  tc qdisc replace dev eth0 root netem delay 10ms loss 1%
  if [ -n "${DOCKLATTICE_NETEM_PROOF:-}" ]; then
    tc qdisc show dev eth0 >"$DOCKLATTICE_NETEM_PROOF"
  else
    printf 'NETEM '
    tc qdisc show dev eth0
  fi
fi

exec /usr/local/bin/transport-prototype "$@"
