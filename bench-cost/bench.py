#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins
"""Per-packet cost benchmark for SPEC.md section 10.2, the pure-Python arm.

Same shape as bench-cost/bench.cpp, with far fewer packets because the
reference Python is two orders of magnitude slower and the point is only the
ratio to C++ on identical input.

    ./bench.py FILE.pcm RATE CHANNELS FRAMES SHIFT PACKETS REPEATS
"""
import array
import json
import os
import statistics
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "python"))
import ubersdr_iq as uiq  # noqa: E402


def main() -> int:
    if len(sys.argv) < 8:
        sys.stderr.write("usage: bench.py FILE.pcm RATE CHANNELS FRAMES SHIFT PACKETS REPEATS\n")
        return 2
    path = sys.argv[1]
    rate, channels, frames = int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
    shift, packets, repeats = int(sys.argv[5]), int(sys.argv[6]), int(sys.argv[7])

    src = array.array("h")
    with open(path, "rb") as f:
        src.frombytes(f.read())
    if sys.byteorder == "big":
        src.byteswap()

    per = frames * channels
    need = per * packets
    tiles = 1
    if len(src) >= need:
        samples = src[:need]
    else:
        whole = (len(src) // per) * per or per
        samples = array.array("h")
        while len(samples) < need:
            samples.extend(src[: min(whole, need - len(samples))])
        tiles = -(-need // whole)

    scaled = shift >= 0
    blocks = [list(samples[p * per:(p + 1) * per]) for p in range(packets)]

    stream = []
    enc_us = []
    for r in range(repeats + 1):  # one warmup
        enc = uiq.StreamEncoder()
        keep = r == 0
        t0 = time.perf_counter()
        for p in range(packets):
            ts = 1700000000000000000 + p * frames * 10**9 // rate
            if scaled:
                pkt = enc.encode_scaled(blocks[p], ts, rate, channels, -999.0, -999.0, shift)
            else:
                pkt = enc.encode(blocks[p], ts, rate, channels, -999.0, -999.0)
            if keep:
                stream.append(pkt)
        t1 = time.perf_counter()
        if r >= 1:
            enc_us.append((t1 - t0) * 1e6 / packets)

    dec_us = []
    for r in range(repeats + 1):
        dec = uiq.StreamDecoder()
        t0 = time.perf_counter()
        for p in range(packets):
            dec.decode(stream[p])
        t1 = time.perf_counter()
        if r >= 1:
            dec_us.append((t1 - t0) * 1e6 / packets)

    wire = sum(len(p) for p in stream)
    pps = rate / frames
    em, dm = statistics.median(enc_us), statistics.median(dec_us)
    print(json.dumps({
        "impl": "python", "file": path, "rate": rate, "channels": channels,
        "frames": frames, "shift": shift, "packets": packets, "repeats": repeats,
        "tiles": tiles, "pps": round(pps, 2),
        "bits_per_sample": round(8.0 * wire / (packets * per), 3),
        "enc_us": round(em, 1), "enc_us_lo": round(min(enc_us), 1),
        "enc_us_hi": round(max(enc_us), 1),
        "dec_us": round(dm, 1), "dec_us_lo": round(min(dec_us), 1),
        "dec_us_hi": round(max(dec_us), 1),
        "enc_core_pct": round(em * pps / 1e4, 3), "dec_core_pct": round(dm * pps / 1e4, 3),
        "enc_msps": round(per / em, 4), "dec_msps": round(per / dm, 4),
    }))
    return 0


if __name__ == "__main__":
    sys.exit(main())
