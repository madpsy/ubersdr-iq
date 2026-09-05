#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Unpack a length-prefixed protocol-version-3 packet capture.

The nine captures in `ka9q_ubersdr/testdata/pcm_predictive/` are the corpus the
published §10.1 was measured on, and they are stored as the packets the
receiver actually sent rather than as a WAV file:

    [payload length uint32 LE][37-byte v3 header][big-endian int16 samples]...

That matters for §10.1 twice over. The samples are big-endian, so they have to
be swapped before anything in this repository will read them; and the packet
boundaries are real -- they are the ones the receiver chose, including the short
remainder packet that ends every 20 ms block at the wide IQ rates -- so they are
recorded rather than reconstructed from a nominal frames-per-packet.

    load_v3.py in.bin out.pcm out.counts

`out.pcm` is raw little-endian int16, `out.counts` one frame count per line, in
order, ready for `spec10 -counts`. A summary goes to stdout.

The source directory is read-only and nothing here writes to it.
"""

import struct
import sys

import numpy as np

FULL_MAGIC = 0x5043
MIN_MAGIC = 0x504D
HDR_V1, HDR_V23, HDR_MIN = 29, 37, 13


def main():
    if len(sys.argv) != 4:
        print(__doc__)
        return 2
    src, out_pcm, out_counts = sys.argv[1:]
    buf = open(src, "rb").read()

    chunks, counts = [], []
    rate = ch = version = None
    quality = []
    pos = 0
    while pos < len(buf):
        (n,) = struct.unpack_from("<I", buf, pos)
        pos += 4
        pkt = buf[pos:pos + n]
        if len(pkt) != n:
            raise SystemExit(f"{src}: truncated packet at {pos}")
        pos += n
        magic = struct.unpack_from("<H", pkt, 0)[0]
        if magic == MIN_MAGIC:
            body = pkt[HDR_MIN:]
        elif magic == FULL_MAGIC:
            version = pkt[2]
            hdr = HDR_V1 if version == 1 else HDR_V23
            rate, ch = struct.unpack_from("<IB", pkt, 20)
            if version >= 2:
                quality.append(struct.unpack_from("<ff", pkt, 25))
            body = pkt[hdr:]
        else:
            raise SystemExit(f"{src}: bad magic 0x{magic:04x} at packet {len(counts)}")
        s = np.frombuffer(body, dtype=">i2").astype("<i2")
        chunks.append(s)
        counts.append(len(s))

    x = np.concatenate(chunks)
    x.tofile(out_pcm)
    frames = [c // ch for c in counts]
    with open(out_counts, "w") as f:
        f.write("\n".join(str(v) for v in frames))

    hist = {}
    for v in frames:
        hist[v] = hist.get(v, 0) + 1
    peak = int(np.max(np.abs(x.astype(np.int32))))
    print(f"{src}: v{version}, {rate} Hz x{ch}, {len(counts)} packets, "
          f"{len(x)} samples, {len(x)/ch/rate:.3f} s, "
          f"frames/packet {dict(sorted(hist.items()))}, peak {peak} "
          f"({20*np.log10(peak/32768):.1f} dBFS), "
          f"rms {np.sqrt(np.mean(x.astype(np.float64)**2)):.1f}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
