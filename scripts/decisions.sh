#!/usr/bin/env bash
# Reads a discovery run's event log (stderr of `cua discover`) on stdin and
# prints one line per model decision as it happens: "  action: reason".
grep --line-buffered agent.decision | sed -u -E 's/.*"action":"([a-z]+)".*"reason":"([^"]*)".*/  \1: \2/'
