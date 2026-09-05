#!/bin/sh
# Would an experimental 10-bit mode for UberSDR-IQ cost FT8 decodes?
#
# Every arm goes through the real codec: samples in, uiq encode, uiq decode,
# samples out. The 16-bit arm must come back bit-identical -- that is the
# control. The others differ only by the front end in requant.py, which
# requantises each packet before the codec sees it and puts the scale back
# afterwards, exactly as a real 10-bit mode would with the scale in the header.
set -e

SRC=../testdata/60s-20m-ft8.wav
RATE=12000
FRAMES=240
PERIOD=15
WORK=work
# 10 bits is the case in question; the rest of the sweep is what puts it in
# context, and shows where the mode stops being free. trunc8 and trunc10 are
# offline controls on whether the per-packet scale earns its complexity -- each
# picks one scale from the whole file's peak, which a live encoder cannot do,
# so they bound rather than compete.
ARMS="bfp8 bfp10 cap10m6 cap10m10 cap10m14 cap10m18"

command -v jt9 >/dev/null || { echo "jt9 not found (install wsjt-x)"; exit 1; }
[ -x ./uiq ] || (cd ../go && go build -o ../bitdepth/uiq ./cmd/uiq)

rm -rf "$WORK"; mkdir -p "$WORK"

python3 wav.py topcm "$SRC" "$WORK/orig.raw.pcm"
# Whole packets only: the encoder drops a trailing partial one, so without this
# the control arm cannot round trip exactly. What is left is 60.12 s, which
# still holds four complete 15 s FT8 periods.
python3 wav.py trim "$WORK/orig.raw.pcm" "$WORK/orig.pcm" $FRAMES
python3 wav.py towav "$WORK/orig.pcm" "$WORK/orig.wav" $RATE

echo
echo "=== requantisation, before the codec sees it ==="
for m in $ARMS; do
    python3 requant.py quantise "$m" "$WORK/orig.pcm" \
        "$WORK/$m.q.pcm" "$WORK/$m.sh" --frames $FRAMES
done

echo
echo "=== through the codec ==="
./uiq encode -rate $RATE -channels 1 -frames $FRAMES \
    "$WORK/orig.pcm" "$WORK/pcm16.uiq" >/dev/null
./uiq decode "$WORK/pcm16.uiq" "$WORK/pcm16.out.pcm" >/dev/null
cmp -s "$WORK/orig.pcm" "$WORK/pcm16.out.pcm" \
    && echo "pcm16   lossless round trip confirmed" \
    || { echo "pcm16   ROUND TRIP FAILED"; exit 1; }
cp "$WORK/orig.wav" "$WORK/pcm16.wav"

for m in $ARMS; do
    ./uiq encode -rate $RATE -channels 1 -frames $FRAMES \
        "$WORK/$m.q.pcm" "$WORK/$m.uiq" >/dev/null
    ./uiq decode "$WORK/$m.uiq" "$WORK/$m.out.q.pcm" >/dev/null
    cmp -s "$WORK/$m.q.pcm" "$WORK/$m.out.q.pcm" \
        || { echo "$m  CODEC ROUND TRIP FAILED"; exit 1; }
    python3 requant.py restore "$WORK/$m.out.q.pcm" "$WORK/$m.sh" \
        "$WORK/$m.out.pcm" --frames $FRAMES
    python3 wav.py towav "$WORK/$m.out.pcm" "$WORK/$m.wav" $RATE
    echo "$m$(printf '%*s' $((8 - ${#m})) '')codec carried the requantised samples exactly"
done

echo
echo "=== wire size ==="
RAW=$(stat -c%s "$WORK/orig.pcm")
printf "%-8s %9s %8s %10s\n" mode bytes ratio kB/s
for f in pcm16 $ARMS; do
    S=$(stat -c%s "$WORK/$f.uiq")
    awk -v n="$f" -v s="$S" -v raw="$RAW" \
        'BEGIN { printf "%-8s %9d %7.2fx %9.2f\n", n, s, raw/s, s/60.12/1000 }'
done

echo
echo "=== FT8 decodes ==="
N=$(python3 wav.py split "$WORK/orig.wav" "$WORK/p" $PERIOD)
for f in pcm16 $ARMS; do
    python3 wav.py split "$WORK/$f.wav" "$WORK/${f}_p" $PERIOD >/dev/null
    : > "$WORK/$f.decodes"
    i=0
    while [ $i -lt "$N" ]; do
        p=$(printf "%02d" $i)
        echo "period $i" >> "$WORK/$f.decodes"
        (cd "$WORK" && jt9 -8 -p $PERIOD -a . -t . "${f}_p_$p.wav" 2>/dev/null) \
            | grep -v DecodeFinished >> "$WORK/$f.decodes" || true
        i=$((i + 1))
    done
done

echo
python3 compare.py "$WORK/pcm16.decodes" $(for f in $ARMS; do echo "$WORK/$f.decodes"; done)
