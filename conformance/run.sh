#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

# Run every implementation against the vectors in testdata/.
#
# Each one decodes the .uiq streams and compares the result with the .pcm
# samples. The Go, Python and C++ implementations also encode the .pcm samples
# and require the result to be byte-identical to the committed .uiq -- see
# testdata/README.md for why that is a stricter test than round-tripping, and
# why the format itself does not require it.
#
# An implementation whose toolchain is missing is skipped, not failed.
#
# Every implementation is run even when an earlier one fails, and the failures
# are named at the end. Stopping at the first would hide the state of the rest,
# which is the state that matters while a change is being carried across four
# languages one at a time.

cd "$(dirname "$0")/.." || exit 1
failed=""

echo "== Go =="
if command -v go >/dev/null 2>&1; then
    (cd go && go test ./...) || failed="$failed Go"
else
    echo "   skipped: no go toolchain"
fi

echo
echo "== Python =="
if command -v python3 >/dev/null 2>&1; then
    python3 python/test_ubersdr_iq.py >/tmp/uiq-python.$$ 2>&1 || failed="$failed Python"
    tail -n 20 /tmp/uiq-python.$$
    rm -f /tmp/uiq-python.$$
else
    echo "   skipped: no python3"
fi

echo
echo "== C++ =="
if command -v c++ >/dev/null 2>&1 || command -v g++ >/dev/null 2>&1; then
    if make -s -C cpp conformance; then
        ./cpp/conformance testdata || failed="$failed C++"
    else
        failed="$failed C++"
    fi
else
    echo "   skipped: no C++ compiler"
fi

echo
echo "== JavaScript =="
if command -v node >/dev/null 2>&1; then
    node js/conformance.mjs testdata || failed="$failed JavaScript"
else
    echo "   skipped: no node"
fi

echo
if [ -n "$failed" ]; then
    echo "FAILED:$failed"
    exit 1
fi
echo "all implementations agree"
