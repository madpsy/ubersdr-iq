#!/bin/sh
# Reproduce the comparison on the committed capture.
#
# The trailing 2.0 is the saturation window per thread count; pass 0 to skip
# that pass, which is most of the runtime.
#
# The `26:` and `15:` arguments are the reduced-depth arms: profile 2 (SPEC.md
# §6.6) driven by the shift schedule ../bitdepth-iq/margin_rule.py chooses for
# that margin in dB. `shifts.py` writes the schedule; `bench` does the encoding.
# 26 dB is the reference sender's default, 15 dB the aggressive end of the range
# it accepts.
set -e
make -s bench

echo "=== shift schedules ==="
python3 shifts.py iq48-14M080.pcm 48000 320 26 sh48-26.bin
python3 shifts.py iq48-14M080.pcm 48000 320 15 sh48-15.bin
for v in iq-384k-mw iq-384k; do
    python3 shifts.py ../testdata/$v.pcm 384000 360 26 sh-$v-26.bin
    python3 shifts.py ../testdata/$v.pcm 384000 360 15 sh-$v-15.bin
done

echo
echo "=== 30 s of 48 kHz IQ, 14.080 MHz ==="
./bench iq48-14M080.pcm 48000 320 3978480 rec_ 5 2.0 26:sh48-26.bin 15:sh48-15.bin
echo

python3 spectrum.py iq48-14M080.pcm 48000 rec_ \
    "UberSDR-IQ, 26 dB margin:rec_uiq_26dB.pcm" \
    "UberSDR-IQ, 15 dB margin:rec_uiq_15dB.pcm"

for v in iq-384k-mw iq-384k; do
    echo
    echo "=== ../testdata/$v.pcm ==="
    ./bench ../testdata/$v.pcm 384000 360 0 ${v}_ 200 2.0 \
        26:sh-$v-26.bin 15:sh-$v-15.bin
    python3 spectrum.py ../testdata/$v.pcm 384000 ${v}_ \
        "UberSDR-IQ, 26 dB margin:${v}_uiq_26dB.pcm" \
        "UberSDR-IQ, 15 dB margin:${v}_uiq_15dB.pcm"
done
