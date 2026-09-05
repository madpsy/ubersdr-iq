#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Would a reduced-depth mode for UberSDR-IQ cost anything on complex baseband?

The IQ half of the question ../bitdepth asked about demodulated audio. Every arm
goes through the real codec -- samples in, `uiq encode`, `uiq decode`, samples
out -- and the round trip is asserted before anything is scored: the lossless
arm must come back bit-identical, and each reduced arm must return exactly the
requantised values it was handed.

Scored on three things, because bytes alone say nothing about what was spent:

- **Spectral margin**, the primary figure. How far the quantisation floor sits
  below the band's own noise floor. Unlike a bit depth this is a promise that
  survives whatever the client does downstream, which matters because an IQ
  stream feeds waterfalls, external SDR software and skimmers with no shared
  idea of what "enough" means.
- **FT8 decodes**, on the captures that carry them, through the whole chain
  including a USB demodulator -- because no client consumes the IQ directly.
- **Bytes.**
"""

import glob
import os
import re
import subprocess
import sys
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import requant_iq as rq
import iqwav

HERE = os.path.dirname(os.path.abspath(__file__))
CAPS = os.path.join(HERE, "captures")
WORK = os.path.join(HERE, "work")
UIQ = os.path.join(HERE, "uiq")
FRAMES = 360                      # what the receiver sends: 1440 B = 720 samples
SPP = FRAMES * 2

# name -> (rate, is_ft8, usb offset within the passband)
CAPTURES = {
    "20m-ft8":  (48000,  True,  0.0),
    "40m-ft8":  (48000,  True,  0.0),
    "20m-192":  (192000, False, 0.0),
    "40m-192":  (192000, False, 0.0),
    "20m-384":  (384000, False, 0.0),
    "mw-909":   (192000, False, 0.0),
    "quiet-6m": (384000, False, 0.0),
}


def find_capture(name):
    w = sorted(glob.glob(os.path.join(CAPS, name, "*.wav")))
    return w[-1] if w else None


def start_epoch(path):
    """First-sample UTC epoch, from the GPS timestamp the recorder names it by."""
    m = re.search(r"_(\d{4}-\d{2}-\d{2})T(\d{2}):(\d{2}):(\d{2})\.(\d{3})Z", path)
    if not m:
        return None
    import datetime as dt
    d = dt.datetime.strptime(m.group(1), "%Y-%m-%d").replace(tzinfo=dt.timezone.utc)
    return (d.timestamp() + int(m.group(2)) * 3600 + int(m.group(3)) * 60
            + int(m.group(4)) + int(m.group(5)) / 1000.0)


def prepare(name):
    """Capture to raw IQ, trimmed to whole packets and, for FT8, to the grid."""
    path = find_capture(name)
    if not path:
        return None
    rate, is_ft8, _ = CAPTURES[name]
    x, wrate = iqwav.read_wav(path)
    if wrate != rate:
        print(f"  ! {name}: expected {rate} Hz, capture is {wrate}")
        rate = wrate

    if is_ft8:
        # Drop the head so the first sample lands on a 15 s UTC boundary, then
        # keep whole 15 s periods. Every arm is cut from this same file, so any
        # residual offset is common to all of them.
        t0 = start_epoch(path)
        if t0 is not None:
            skip_s = (-t0) % 15.0
            x = x[int(round(skip_s * rate)) * 2:]
            nper = int(len(x) / 2 // (15 * rate))
            x = x[:nper * 15 * rate * 2]
    n = (len(x) // SPP) * SPP
    return x[:n], rate


def code(tag, q, rate):
    """Through the real encoder and back. Returns bytes, or None if it failed."""
    qp = os.path.join(WORK, f"{tag}.q.pcm")
    up = os.path.join(WORK, f"{tag}.uiq")
    op = os.path.join(WORK, f"{tag}.out.pcm")
    q.tofile(qp)
    for cmd in (
        [UIQ, "encode", "-rate", str(rate), "-channels", "2",
         "-frames", str(FRAMES), qp, up],
        [UIQ, "decode", up, op],
    ):
        r = subprocess.run(cmd, capture_output=True)
        if r.returncode != 0:
            print(f"  ! {tag}: {r.stderr.decode()[:200]}")
            return None
    if open(qp, "rb").read() != open(op, "rb").read():
        print(f"  ! {tag}: ROUND TRIP FAILED")
        return None
    return os.path.getsize(up)


def decodes(tag, x, rate):
    """FT8 messages recovered from this arm, through a USB demodulator.

    Returns {(period, message): snr}, and the set of messages that only came
    back with the help of a priori information. jt9 marks those `a1`, and they
    are tracked apart because a message that needs AP has degraded even though
    it still appears in the list.
    """
    a = iqwav.usb_demod(x, rate)
    got, ap = {}, set()
    per = 12000 * 15
    for i in range(len(a) // per):
        wp = os.path.join(WORK, f"{tag}.p{i}.wav")
        iqwav.write_wav(wp, a[i * per:(i + 1) * per], 12000)
        r = subprocess.run(["jt9", "-8", wp], capture_output=True, cwd=WORK)
        for line in r.stdout.decode(errors="replace").splitlines():
            f = line.split()
            if len(f) < 6:
                continue
            try:
                snr = int(f[1])
            except ValueError:
                continue
            msg = " ".join(f[5:]).strip()
            if msg.endswith(" a1"):
                msg = msg[:-3].strip()
                ap.add((i, msg))
            got[(i, msg)] = snr
    return got, ap


def main():
    os.makedirs(WORK, exist_ok=True)
    if not os.path.exists(UIQ):
        subprocess.run(["go", "build", "-o", UIQ, "./cmd/uiq"],
                       cwd=os.path.join(HERE, "..", "go"), check=True)

    arms = ([("bfp%d" % b, ("bfp", b)) for b in (12, 10, 9, 8)]
            + [("cap10d%d" % d, ("cap", d)) for d in (0, 1, 2, 3)]
            + [("cap10free", ("cap", None))])

    for name in CAPTURES:
        prepared = prepare(name)
        if prepared is None:
            print(f"{name}: no capture, skipped")
            continue
        x, rate = prepared
        rate_i, is_ft8, _ = CAPTURES[name]
        print(f"\n=== {name}  {rate} Hz  {len(x)//2} frames  "
              f"{len(x)/2/rate:.1f} s  peak {int(np.max(np.abs(x)))}")

        b16 = code(f"{name}.pcm16", x, rate)
        if b16 is None:
            continue
        base, base_ap = decodes(f"{name}.pcm16", x, rate) if is_ft8 else (None, None)
        extra = f"  {len(base)} decodes" if base is not None else ""
        print(f"  {'lossless':10s} {b16:9d} B{extra}")

        for tag, (kind, p) in arms:
            if kind == "bfp":
                q, sh = rq.quantise_bfp(x, SPP, p)
            else:
                q, sh = rq.quantise_cap(x, SPP, 10, 14.0, rate, delta=p)
            by = code(f"{name}.{tag}", q, rate)
            if by is None:
                continue
            r = rq.restore(q, sh, SPP)
            m = rq.spectral_margin(x, r)
            line = (f"  {tag:10s} {by:9d} B  {1-by/b16:6.1%}  "
                    f"margin {m:+6.1f} dB  snr {rq.time_snr(x, r):5.1f} dB  "
                    f"shift {sh.min()}-{sh.max()} (mean {sh.mean():.2f})")
            if base is not None:
                got, ap = decodes(f"{name}.{tag}", r, rate)
                kept = set(got) & set(base)
                # Out of the decodes that did come back, how many did jt9 report
                # at exactly the lossless arm's strength. jt9 reports SNR in
                # whole dB, so anything outside this is one bin away -- it is a
                # margin measure, not a success rate. `lost` is the success rate.
                same = sum(1 for k in kept if got[k] == base[k])
                line += (f"  kept {len(kept)}/{len(base)}"
                         f"  lost {len(base)-len(kept)}"
                         f"  AP {len(ap - base_ap)}"
                         f"  same-snr {same}/{len(kept)}")
                if set(got) - set(base):
                    line += f"  (+{len(set(got)-set(base))} new)"
            print(line)


if __name__ == "__main__":
    sys.exit(main())
