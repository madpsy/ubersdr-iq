#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""UberSDR-IQ against FLAC, lossless, on one recording.

Five arms on the same samples:

  1. this codec, packetised the way the receiver sends it, per-packet headers
     counted -- the number a deployment actually pays;
  2. flac -5, default block size: whole-file compression with look-ahead over
     the entire recording, which is FLAC at its best;
  3. flac -8, default block size. NOT -9: the reference encoder, flac 1.4.3,
     refuses -9 -- "compression level '9' is reserved" -- and -8 is what it
     calls --best. Two further arms add -e -p on top of -8, which is the most
     the reference encoder will do to a file and is slower still, so that
     nothing is left on FLAC's side of the table;
  4. flac -5 with the block size forced to this codec's packet size;
  5. flac -9 likewise.

Arms 2 and 3 answer "what does the streaming constraint cost us"; arms 4 and 5
answer "do we win when FLAC is held to the same constraint". Both belong in the
result. Neither is "the fair one".

Every arm is verified lossless by decoding it and comparing sample for sample
against the input, and every arm is timed by the clock in both directions.

    flaccmp.py in.pcm RATE FRAMES_PER_PACKET outdir [CHANNELS]

CHANNELS defaults to 1. At 2 the samples are interleaved I/Q and FLAC is handed
them as a stereo WAV, which lets it try mid-side decorrelation across I and Q --
a different trick from this codec's complex tap, and worth seeing.
"""

import json
import os
import struct
import subprocess
import sys
import time
import wave

import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
UIQ = os.path.join(HERE, "work", "uiq-current")


def wall(cmd, reps=3, **kw):
    """Best of `reps` wall-clock seconds. Best, not mean: it is the one that
    is least contaminated by whatever else the machine was doing."""
    best = None
    for _ in range(reps):
        t = time.perf_counter()
        r = subprocess.run(cmd, capture_output=True, **kw)
        d = time.perf_counter() - t
        if r.returncode != 0:
            raise SystemExit(f"{cmd[0]}: {r.stderr.decode()[:300]}")
        best = d if best is None else min(best, d)
    return best, r


def flac_metadata_bytes(path):
    """Size of everything before the first audio frame: "fLaC" plus the
    metadata blocks, itemised.

    It is quoted separately because a streaming deployment cannot simply drop
    it -- the decoder needs the rate, the depth and the channel count from
    somewhere -- but it is a one-off, where this codec's equivalent rides every
    resynchronisation packet.
    """
    b = open(path, "rb").read()
    if b[:4] != b"fLaC":
        raise SystemExit(f"{path}: not a FLAC stream")
    pos, blocks = 4, {}
    while True:
        hdr = b[pos]
        last, btype = hdr >> 7, hdr & 0x7F
        ln = int.from_bytes(b[pos + 1:pos + 4], "big")
        name = {0: "STREAMINFO", 1: "PADDING", 2: "APPLICATION", 3: "SEEKTABLE",
                4: "VORBIS_COMMENT", 5: "CUESHEET", 6: "PICTURE"}.get(btype, str(btype))
        blocks[name] = blocks.get(name, 0) + 4 + ln
        pos += 4 + ln
        if last:
            break
    return pos, blocks


def read_wav_pcm(path):
    with wave.open(path, "rb") as w:
        assert w.getsampwidth() == 2
        return np.frombuffer(w.readframes(w.getnframes()), dtype="<i2")


def main():
    src, rate, fpp, out = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
    ch = int(sys.argv[5]) if len(sys.argv) > 5 else 1
    os.makedirs(out, exist_ok=True)
    x = np.fromfile(src, dtype="<i2")
    per = fpp * ch
    n = (len(x) // per) * per          # whole packets, so every arm sees the
    x = x[:n]                          # same samples
    packets = n // per
    dur = n / ch / rate
    raw = n * 2
    base = os.path.join(out, "src")
    x.tofile(base + ".pcm")
    with wave.open(base + ".wav", "wb") as w:
        w.setnchannels(ch)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(x.tobytes())
    print(f"{src}: {n} samples, {dur:.3f} s at {rate} Hz x{ch}, {packets} "
          f"packets of {fpp} frames, raw {raw} bytes")

    arms = []

    # --- arm 1: this codec ------------------------------------------------
    up = base + ".uiq"
    enc_t, r = wall([UIQ, "encode", "-rate", str(rate), "-channels", str(ch),
                     "-frames", str(fpp), base + ".pcm", up])
    # The container writes a 4-byte length before each packet. A message
    # transport supplies that boundary itself, so the bytes a deployment sends
    # are the packets alone.
    coded = os.path.getsize(up) - 4 * packets
    dec_t, _ = wall([UIQ, "decode", up, base + ".out.pcm"])
    ok = open(base + ".pcm", "rb").read() == open(base + ".out.pcm", "rb").read()
    arms.append(dict(arm="ubersdr-iq lossless", detail=f"{fpp}-sample packets, "
                     "per-packet headers counted", bytes=coded,
                     container_bytes=os.path.getsize(up),
                     metadata_bytes=0, encode_s=enc_t, decode_s=dec_t,
                     lossless=ok, verified="decoded stream compared byte for "
                     "byte against the input samples"))

    # --- arms 2-5: FLAC ---------------------------------------------------
    for level, extra in ((5, []), (8, []), (8, ["-e", "-p"])):
        for bs in (None, fpp):
            suffix = "".join(extra)
            tag = (f"flac{level}{suffix}"
                   + ("" if bs is None else f".bs{bs}"))
            fp = os.path.join(out, tag + ".flac")
            cmd = ["flac", f"-{level}"] + extra + [
                "-f", "--no-padding", "--no-seektable", "-s", "-o", fp,
                base + ".wav"]
            if bs is not None:
                cmd.insert(2, f"--blocksize={bs}")
            enc_t, _ = wall(cmd)
            meta, blocks = flac_metadata_bytes(fp)
            total = os.path.getsize(fp)
            dp = os.path.join(out, tag + ".dec.wav")
            dec_t, _ = wall(["flac", "-d", "-f", "-s", "-o", dp, fp])
            ok = np.array_equal(read_wav_pcm(dp), x)
            # `flac -t` is the encoder's own verification, run as well as the
            # sample comparison rather than instead of it.
            t = subprocess.run(["flac", "-t", "-s", fp], capture_output=True)
            arms.append(dict(
                arm=f"flac -{level}" + (" " + " ".join(extra) if extra else "")
                    + ("" if bs is None else f" --blocksize={bs}"),
                detail=("default block size (4096)" if bs is None
                        else f"block size forced to {bs} = one packet"),
                bytes=total - meta, container_bytes=total,
                metadata_bytes=meta, metadata_blocks=blocks,
                encode_s=enc_t, decode_s=dec_t, lossless=ok and t.returncode == 0,
                verified="decoded to WAV and compared sample for sample; "
                         "`flac -t` also passed"))
            os.remove(dp)

    print()
    print(f"{'arm':34s} {'frames B':>10s} {'meta B':>7s} {'total B':>10s} "
          f"{'kB/s':>8s} {'ratio':>7s} {'enc s':>7s} {'dec s':>7s}  lossless")
    for a in arms:
        print(f"{a['arm']:34s} {a['bytes']:10d} {a['metadata_bytes']:7d} "
              f"{a['bytes']+a['metadata_bytes']:10d} "
              f"{(a['bytes']+a['metadata_bytes'])/dur/1000:8.2f} "
              f"{raw/(a['bytes']+a['metadata_bytes']):6.3f}x "
              f"{a['encode_s']:7.3f} {a['decode_s']:7.3f}  {a['lossless']}")

    with open(os.path.join(out, "arms.json"), "w") as f:
        json.dump(dict(source=src, rate=rate, frames_per_packet=fpp,
                       samples=n, duration_s=dur, raw_bytes=raw,
                       packets=packets, arms=arms), f, indent=1)
    for f in (base + ".pcm", base + ".out.pcm", base + ".wav", up):
        os.remove(f)
    return 0


if __name__ == "__main__":
    sys.exit(main())
