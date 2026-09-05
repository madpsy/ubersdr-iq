#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Write the per-packet shift schedule for a reduced-depth arm.

The schedule is not invented here. It is `../bitdepth-iq/margin_rule.py`'s
`quantise_margin`, which is the policy the reference sender uses: a client asks
for a margin in decibels, and the depth that honours it is worked out per packet
from the packet's crest factor and the band's spectral peakiness. Only the shift
it chooses is taken; the quantisation itself is left to the encoder, which does
what SPEC.md §6.6.2 specifies rather than what a numpy experiment happens to do.

Output is one unsigned byte per packet, in packet order, for `bench` to hand to
`StreamEncoder::EncodeScaled`. `bench` drops a trailing partial packet, so the
schedule is generated over the same truncated sample array it will encode.

    python3 shifts.py FILE.pcm RATE FRAMES_PER_BLOCK MARGIN_DB OUT.bin
"""

import os
import sys

import numpy as np

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                "..", "bitdepth-iq"))
import margin_rule  # noqa: E402

path, rate, fpb, margin, out = (sys.argv[1], int(sys.argv[2]), int(sys.argv[3]),
                                float(sys.argv[4]), sys.argv[5])

x = np.fromfile(path, dtype="<i2")
spp = fpb * 2                      # samples per packet: two per frame
nblk = len(x) // spp
x = x[:nblk * spp]                 # what bench encodes

_, sh, depth = margin_rule.quantise_margin(x, spp, margin, rate)
sh = sh[:nblk].astype(np.uint8)
sh.tofile(out)

d = depth[:nblk]
print(f"{os.path.basename(path)} @ {margin:.0f} dB: {nblk} packets, "
      f"shift min {sh.min()} max {sh.max()} mean {sh.mean():.3f}, "
      f"depth min {d.min():.0f} max {d.max():.0f} mean {d.mean():.3f} bits "
      f"-> {out}")
