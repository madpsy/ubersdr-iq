#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins
"""Unpack the captures run.sh needs into raw little-endian int16 PCM.

Two sources. The nine reference captures of section 10 -- 600 consecutive
packets each, at the packetisation the receiver really uses -- live in the
receiver tree as length-prefixed protocol version 3 packets with big-endian
samples; UBERSDR_TREE says where that tree is. The 15 s wideband WAV captures
in bitdepth-iq/captures are in this repository. The benchmark reads raw PCM so
that nothing in the timed path is parsing a container.

    ./prep.py [OUTDIR]     (default: bench-cost/data)

    UBERSDR_TREE=/path/to/ka9q_ubersdr ./prep.py

Without UBERSDR_TREE only the wideband arms can be prepared, and run.sh will
skip the rest.
"""
import array
import collections
import glob
import os
import struct
import sys
import wave

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.join(HERE, "..")

JOBS = [
    ("bitdepth-iq/captures/20m-384", "iq-384k-20m.pcm"),
    ("bitdepth-iq/captures/quiet-6m", "iq-384k-quiet6m.pcm"),
    ("bitdepth-iq/captures/mw-909", "iq-192k-mw909.pcm"),
    ("testdata/60s-20m-ft8.wav", "audio-12k-60s.pcm"),
]


# The version 3 packet header the capture tool wrote, ahead of the samples.
V3_HEADER = 37


def unpack_reference(tree: str, out: str) -> None:
    """Split the receiver tree's nine reference captures into raw PCM.

    A capture is [uint32 length][packet] repeated. Packets whose length is not
    the dominant one are dropped: the wideband files begin with a few packets
    from before the mode change, and mixing sample counts would change what is
    being timed.
    """
    src = os.path.join(tree, "testdata", "pcm_predictive")
    for f in sorted(glob.glob(os.path.join(src, "*.bin"))):
        raw = open(f, "rb").read()
        off, pkts = 0, []
        while off < len(raw):
            (n,) = struct.unpack_from("<I", raw, off)
            off += 4
            pkts.append(raw[off:off + n])
            off += n
        dom = collections.Counter(len(p) for p in pkts).most_common(1)[0][0]
        a, kept = array.array("h"), 0
        for p in pkts:
            if len(p) != dom:
                continue
            b = array.array("h")
            b.frombytes(p[V3_HEADER:])
            b.byteswap()  # the capture is big-endian; the benchmark reads little
            a.extend(b)
            kept += 1
        name = os.path.basename(f).replace(".bin", ".pcm")
        with open(os.path.join(out, name), "wb") as fh:
            fh.write(a.tobytes())
        print(f"{name}: {kept}/{len(pkts)} packets of {(dom - V3_HEADER) // 2} samples")


def main() -> int:
    out = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "data")
    os.makedirs(out, exist_ok=True)
    tree = os.environ.get("UBERSDR_TREE")
    if tree and os.path.isdir(os.path.join(tree, "testdata", "pcm_predictive")):
        unpack_reference(tree, out)
    else:
        print("UBERSDR_TREE unset or has no testdata/pcm_predictive: "
              "the nine reference captures were not prepared")
    for src, name in JOBS:
        p = os.path.join(ROOT, src)
        if not p.endswith(".wav"):
            found = sorted(glob.glob(os.path.join(p, "*.wav")))
            if not found:
                print(f"{src}: no capture, skipped")
                continue
            p = found[0]
        with wave.open(p) as w:
            if w.getsampwidth() != 2:
                print(f"{p}: not 16-bit, skipped")
                continue
            data = w.readframes(w.getnframes())
            with open(os.path.join(out, name), "wb") as f:
                f.write(data)
            print(f"{name}: {w.getframerate()} Hz, {w.getnchannels()} ch, "
                  f"{w.getnframes()} frames")
    return 0


if __name__ == "__main__":
    sys.exit(main())
