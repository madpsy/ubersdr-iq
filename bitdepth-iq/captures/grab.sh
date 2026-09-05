#!/bin/bash
# Capture the IQ set for the reduced-depth experiment. Sequential on purpose:
# one session at a time so a 384 kHz stream is not sharing the link with another.
OUT=/home/nathan/repos/ubersdr-iq/bitdepth-iq/captures
REC=/home/nathan/repos/ka9q_ubersdr/clients/iq-recorder/build/iq-recorder
HP="-host ${UBERSDR_HOST:?set UBERSDR_HOST} -port ${UBERSDR_PORT:-8080}"
run() {
  echo "=== $1  $2 Hz  $3  $4 s  (start $(date -u +%H:%M:%S)Z)"
  mkdir -p "$OUT/$1"
  $REC $HP -frequency "$2" -mode "$3" -duration "$4" -output-dir "$OUT/$1" 2>&1 | sed 's/^/    /'
}
# The FT8 pair runs 90 s so that five whole 15 s periods survive trimming to the
# first UTC boundary. Every arm is cut from the same trimmed file, so whatever
# alignment error remains is common to all of them and cancels in the comparison.
run 20m-ft8  14074000 iq48  90
run 40m-ft8   7074000 iq48  90
run 20m-192  14175000 iq192 30
run 40m-192   7100000 iq192 30
run 20m-384  14175000 iq384 30
run mw-909     909000 iq192 30
run quiet-6m 50100000 iq384 30
echo "=== done $(date -u +%H:%M:%S)Z"
