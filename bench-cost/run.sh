#!/bin/bash
# Reproduce the section 10.2 cost table.
#
# Needs the wideband captures unpacked from bitdepth-iq/captures into raw
# little-endian int16 PCM. DATA is where those live; prep.py writes them.
#
#   ./prep.py /path/to/data && DATA=/path/to/data ./run.sh
#
# Pinned to one core so nothing migrates mid-run; every arm is single-threaded.
set -e
cd "$(dirname "$0")"
DATA=${DATA:-data}
CORE=${CORE:-4}
PIN="taskset -c $CORE"

make -s bench bench-noleak
(cd go && go build -o ../bench-go .)

# The nine reference captures of section 10 are 600 consecutive packets each,
# taken from a live receiver with the packetisation the receiver actually uses:
# 240 samples for 12 kHz audio, 480 for 24 kHz audio and 12 kHz IQ, 720 for
# 384 kHz IQ. They are the same captures section 10.1 measures compression on.
# The two long arms at the end are the 15 s wideband captures in
# bitdepth-iq/captures, as a check that a 600-packet run is long enough.
#
# arm  file  rate  channels  frames/packet  shift  packets  repeats
arms() {
cat <<EOF
a12-cw            $DATA/cw-14025.pcm            12000 1 240 -1   600 25
a12-lsb-voice     $DATA/lsb-voice-7150.pcm      12000 1 240 -1   600 25
a12-usb-ft8       $DATA/usb-ft8-14074.pcm       12000 1 240 -1   600 25
a24-am            $DATA/am-14074.pcm            24000 1 480 -1   600 25
a24-nfm           $DATA/nfm-14074.pcm           24000 1 480 -1   600 25
iq12-ft8          $DATA/iq12k-ft8-14074.pcm     12000 2 240 -1   600 25
iq384-ft8         $DATA/iq384-ft8-14074.pcm    384000 2 360 -1   573 25
iq384-mw          $DATA/iq384-mw-carriers.pcm  384000 2 360 -1   573 25
iq384-quiet       $DATA/iq384-quiet-band.pcm   384000 2 360 -1   573 25
iq12-ft8-p2s6     $DATA/iq12k-ft8-14074.pcm     12000 2 240  6   600 25
iq384-ft8-p2s6    $DATA/iq384-ft8-14074.pcm    384000 2 360  6   573 25
iq384-mw-p2s6     $DATA/iq384-mw-carriers.pcm  384000 2 360  6   573 25
iq384-quiet-p2s6  $DATA/iq384-quiet-band.pcm   384000 2 360  6   573 25
iq384-ft8-p2s4    $DATA/iq384-ft8-14074.pcm    384000 2 360  4   573 25
iq384-ft8-p2s10   $DATA/iq384-ft8-14074.pcm    384000 2 360 10   573 25
long-384-20m      $DATA/iq-384k-20m.pcm        384000 2 360 -1 16000  9
long-384-quiet6m  $DATA/iq-384k-quiet6m.pcm    384000 2 360 -1 16000  9
long-192-mw909    $DATA/iq-192k-mw909.pcm      192000 2 360 -1 15000  9
long-a12-ft8      $DATA/audio-12k-60s.pcm       12000 1 240 -1  3000  9
EOF
}

echo "# uname: $(uname -srm)"
echo "# cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //')"
echo "# loadavg at start: $(cut -d' ' -f1-3 /proc/loadavg)"
arms | while read -r arm f rate ch fr sh pk rp; do
    [ -z "$arm" ] && continue
    for bin in ./bench ./bench-go ./bench-noleak; do
        printf '%s\t%s\t' "$arm" "$(basename $bin)"
        $PIN $bin "$f" "$rate" "$ch" "$fr" "$sh" "$pk" "$rp"
    done
done
echo "# loadavg at end: $(cut -d' ' -f1-3 /proc/loadavg)"
