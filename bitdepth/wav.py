#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Move between WSJT-X's .wav files and the raw int16 the codec tools take.

    wav.py topcm in.wav out.pcm
    wav.py towav in.pcm out.wav [rate]
    wav.py split in.wav prefix seconds     -> prefix_00.wav, prefix_01.wav, ...
    wav.py trim in.pcm out.pcm frames      -> whole packets only

WSJT-X wants mono 16-bit PCM at 12000 Hz, one Rx period per file.
"""

import sys
import wave
import numpy as np


def main() -> int:
    if len(sys.argv) < 4:
        print(__doc__)
        return 2
    cmd = sys.argv[1]

    if cmd == "topcm":
        with wave.open(sys.argv[2], "rb") as w:
            assert w.getnchannels() == 1 and w.getsampwidth() == 2, "want mono int16"
            np.frombuffer(w.readframes(w.getnframes()), dtype="<i2").tofile(sys.argv[3])
        return 0

    if cmd == "towav":
        rate = int(sys.argv[4]) if len(sys.argv) > 4 else 12000
        x = np.fromfile(sys.argv[2], dtype="<i2")
        with wave.open(sys.argv[3], "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(rate)
            w.writeframes(x.tobytes())
        return 0

    if cmd == "trim":
        # The encoder drops a trailing partial packet, so a control arm that
        # must round trip exactly has to start on a whole number of them.
        x = np.fromfile(sys.argv[2], dtype="<i2")
        frames = int(sys.argv[4])
        n = (len(x) // frames) * frames
        x[:n].tofile(sys.argv[3])
        print(f"trimmed {len(x)} -> {n} samples, {n // frames} packets")
        return 0

    if cmd == "split":
        secs = float(sys.argv[4])
        with wave.open(sys.argv[2], "rb") as w:
            rate = w.getframerate()
            x = np.frombuffer(w.readframes(w.getnframes()), dtype="<i2")
        n = int(secs * rate)
        for i in range(len(x) // n):
            with wave.open(f"{sys.argv[3]}_{i:02d}.wav", "wb") as o:
                o.setnchannels(1)
                o.setsampwidth(2)
                o.setframerate(rate)
                o.writeframes(x[i * n:(i + 1) * n].tobytes())
        print(len(x) // n)
        return 0

    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main())
