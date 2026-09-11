#!/bin/sh
# Fake acli for tests: records its argv (FAKE_ACLI_ARGS) and prints the search fixture.
if [ -n "$FAKE_ACLI_ARGS" ]; then
  printf '%s\n' "$@" > "$FAKE_ACLI_ARGS"
fi
if [ "$1 $2 $3" != "jira workitem search" ]; then
  echo "fake-acli: unsupported command: $*" >&2
  exit 2
fi
dir=$(dirname "$0")
cat "${FAKE_ACLI_FIXTURE:-$dir/jira-search.json}"
