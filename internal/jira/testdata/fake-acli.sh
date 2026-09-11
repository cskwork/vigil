#!/bin/sh
# Fake acli for jira package tests. Records every invocation's argv (one per
# line, calls separated by a blank line) in FAKE_ACLI_LOG. `comment create`
# appends the body to FAKE_ACLI_STORE unless FAKE_ACLI_LOSE_WRITE=1 (simulates
# a write that reports success without effect); `comment list` prints the store.
if [ -n "$FAKE_ACLI_LOG" ]; then
  { printf '%s\n' "$@"; echo; } >> "$FAKE_ACLI_LOG"
fi
if [ "$1 $2 $3" != "jira workitem comment" ]; then
  echo "fake-acli: unsupported command: $*" >&2
  exit 2
fi
store="${FAKE_ACLI_STORE:?FAKE_ACLI_STORE required}"
case "$4" in
  create)
    body=""
    while [ $# -gt 0 ]; do
      if [ "$1" = "--body" ]; then body="$2"; shift; fi
      shift
    done
    if [ -z "$FAKE_ACLI_LOSE_WRITE" ]; then
      # store one JSON string per line (escape backslashes, quotes and newlines)
      printf '%s' "$body" | awk 'BEGIN{ORS=""} {gsub(/\\/,"\\\\"); gsub(/"/,"\\\""); if (NR>1) printf "\\n"; printf "%s", $0} END{print "\n"}' >> "$store"
    fi
    n=$(wc -l < "$store" | tr -d ' ')
    echo "{\"id\":\"1000$n\"}"
    ;;
  list)
    echo '{"comments":['
    i=0
    if [ -s "$store" ]; then
      while IFS= read -r line; do
        i=$((i+1))
        [ $i -gt 1 ] && echo ','
        printf '{"id":"1000%s","body":"%s","author":"fake"}' "$i" "$line"
      done < "$store"
    fi
    echo "],\"total\":$i}"
    ;;
  *)
    echo "fake-acli: unsupported comment op: $4" >&2
    exit 2
    ;;
esac
