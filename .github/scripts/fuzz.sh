#!/usr/bin/env bash
#
# Run one fuzz target, failing the job on real findings while tolerating a
# known shutdown race in Go's own fuzzing tooling.
#
# Usage: fuzz.sh <package> <FuzzTarget> <duration>
#
# WHY THIS EXISTS
#
# When the fuzz time budget expires while a worker call is in flight, the
# coordinator sees a pipe error, finds its context already expired, and returns
# that context error as a TEST FAILURE rather than a clean stop:
#
#   internal/fuzz/worker.go:
#       entry, resp, isInternalError, err := w.client.fuzz(ctx, input.entry, args)
#       if err != nil {
#               w.stop()
#               if ctx.Err() != nil {
#                       return ctx.Err()        // "context deadline exceeded"
#               }
#
# The result is a red build with no failing input, no crasher file, and nothing
# to fix. It is most likely for very fast targets, because millions of
# executions across many workers means a high chance that one is mid-call at the
# instant the deadline lands — the gossip control-message parsers run about
# 5 million executions in 30 seconds and hit it first.
#
# WHAT THIS DOES NOT DO
#
# It does not retry, and it does not swallow failures. A real finding writes a
# crasher to testdata/fuzz/<Target>/ and prints the failing input; both are
# checked for explicitly and either one fails the job. The ONLY tolerated case
# is: non-zero exit, no NEW crasher file, and the sole reported error being the
# context deadline. Anything else — a panic, a non-canonical encoding, an
# unexpected message — fails loudly, as it must.
#
# "New" matters: past findings are committed to testdata/fuzz/<Target>/ as
# regression corpus, so that directory is often non-empty at checkout. Treating
# it as non-empty-means-crasher reports every committed regression seed as a
# fresh finding. We snapshot the directory before the run and compare after, so
# only files this run produced count.
set -uo pipefail

pkg="$1"
target="$2"
duration="$3"

# Where `go test` writes a crasher, relative to the package directory.
crasher_dir="${pkg#./}/testdata/fuzz/${target}"

log="$(mktemp)"
before="$(mktemp)"
after="$(mktemp)"
trap 'rm -f "$log" "$before" "$after"' EXIT

# Committed regression corpus, i.e. everything that is NOT a finding from this run.
ls -A "$crasher_dir" 2>/dev/null | sort > "$before"

# The pattern is ANCHORED. -fuzz takes a regular expression, and Go refuses to
# run at all when it matches more than one target:
#
#   testing: will not fuzz, -fuzz matches more than one fuzz test:
#           [FuzzUnmarshalFileBody FuzzUnmarshal]
#
# So an unanchored name silently becomes a landmine for whoever adds the next
# target — adding FuzzUnmarshalFileBody broke the FuzzUnmarshal step, in a
# different file, with an error that names neither the caller nor the fix. The
# argument stays a bare name because it is also the corpus directory below.
set +e
go test "$pkg" -run XXX -fuzz "^${target}\$" -fuzztime "$duration" 2>&1 | tee "$log"
status="${PIPESTATUS[0]}"
set -e

if [ "$status" -eq 0 ]; then
  exit 0
fi

echo "::group::${target} exited ${status}; classifying"

ls -A "$crasher_dir" 2>/dev/null | sort > "$after"
new_crashers="$(comm -13 "$before" "$after")"

if [ -n "$new_crashers" ]; then
  echo "::endgroup::"
  echo "::error::${target} found a real crasher. New files in ${crasher_dir}:"
  echo "$new_crashers"
  exit 1
fi

if ! grep -q "context deadline exceeded" "$log"; then
  echo "::endgroup::"
  echo "::error::${target} failed for a reason other than the known shutdown race."
  exit 1
fi

# "context deadline exceeded" must be the ONLY failure. A run that also reports
# a genuine problem must not be excused by the presence of the race message.
if grep -qE "^\s+(Failing input written to|.*\.go:[0-9]+:)" "$log"; then
  echo "::endgroup::"
  echo "::error::${target} reported a failing input alongside the deadline message."
  exit 1
fi

echo "::endgroup::"
echo "::warning::${target} hit the Go fuzzing shutdown race (no crasher produced); treating as a pass."
exit 0
