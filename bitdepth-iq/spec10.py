#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Regenerate SPEC.md §10.1 (compression) and §10.3 (Rice parameter range).

Ten streams, each one a live capture whose contents were verified before it was
measured; see captures/grab_spec10.sh for how they were taken and why those
frequencies. Every figure printed here comes out of the codec in ../go, through
`spec10/`, and every arm round trips before it is scored.

The IQ streams get four arms, because profile 2 (§6.6) is now in production and
a lossless-only table no longer describes what the receiver sends:

  lossless   profile 1, the complex predictor, nothing requantised
  m26        profile 2 at a 26 dB margin -- the reference sender's default
  m15        profile 2 at a 15 dB margin -- the aggressive arm
  m10        profile 2 at a 10 dB margin -- the floor a client may ask for

Demodulated audio gets the lossless arm alone. Profile 2 is complex-only: the
encoder refuses it at one channel, so a margin arm for audio would be measuring
something no server can send.

The shift is never chosen here. It comes from margin_rule.quantise_margin, the
margin-to-depth policy, from the margin alone -- which is the point of the
design, and the reason the achieved margin is reported next to the bytes.
"""

import glob
import json
import os
import subprocess
import sys

import numpy as np
import zstandard

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import iqwav
import margin_rule as mr
import requant_iq as rq

HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.path.join(HERE, "work", "spec10")
DRIVER = os.path.join(HERE, "work", "spec10bin")
AUD = os.path.join(HERE, "captures", "audio")

# The nine captures the published §10.1 was measured on, unpacked out of
# ka9q_ubersdr/testdata/pcm_predictive by captures/load_v3.py. That directory is
# read-only; nothing here writes to it. Because those files ARE the packets the
# receiver sent, each carries an exact frame count per packet and none of the
# packetisation has to be inferred.
ORIG = os.path.join(WORK, "orig")

# The margin the reference sender defaults to, an aggressive arm, and the floor
# of the range a client may ask for (pcm_lossy.go: lossyMinMarginDB 10,
# lossyMaxMarginDB 60).
MARGINS = (26, 15, 10)

# The receiver's own ceiling on quantiser depth, from pcm_lossy.go
# (lossyMaxDepth). margin_rule.py's own default is 14, which predates it.
MAX_DEPTH = 17

# A 20 ms block at 384 kHz leaves the server as 21 packets of 360 frames and
# one of 120 -- measured on the wire, 31501 and 1500 over a 30 s capture, and
# the 95%/5% split §5.8 describes. Everything else is one packet per block.
PAT_384 = "360x21,120"

# The same thing at every other IQ rate. A 20 ms block is `rate/50` frames and
# the server cuts it into 360-frame packets with one short remainder; all five
# were counted on the wire during the captures rather than derived, and all
# five come out exactly as the arithmetic says.
#
#   12 kHz    240 frames   = one packet of 240
#   48 kHz    960 frames   = 2x360 + 240
#   96 kHz   1920 frames   = 5x360 + 120
#  192 kHz   3840 frames   = 10x360 + 240
#  384 kHz   7680 frames   = 21x360 + 120
PAT_12 = "240"
PAT_48 = "360x2,240"
PAT_96 = "360x5,120"
PAT_192 = "360x10,240"


def wav_samples(path):
    x, rate = iqwav.read_wav(path)
    return np.asarray(x, dtype="<i2"), rate


def pcm_samples(path):
    return np.fromfile(path, dtype="<i2")


STREAMS = [
    # (key, table label, rate column, source, channels, pattern, provenance)
    dict(key="cw", row="CW", rate=12000, chan=1, pat="240",
         rate_col="12 kHz audio", src=os.path.join(AUD, "cw.pcm"),
         meta=os.path.join(AUD, "cw.json")),
    dict(key="ssbvoice", row="SSB voice", rate=12000, chan=1, pat="240",
         rate_col="12 kHz audio", src=os.path.join(AUD, "ssbvoice.pcm"),
         meta=os.path.join(AUD, "ssbvoice.json")),
    dict(key="ssbdata", row="SSB data", rate=12000, chan=1, pat="240",
         rate_col="12 kHz audio", src=os.path.join(AUD, "ssbdata.pcm"),
         meta=os.path.join(AUD, "ssbdata.json")),
    dict(key="am", row="AM", rate=24000, chan=1, pat="480",
         rate_col="24 kHz audio", src=os.path.join(AUD, "am.pcm"),
         meta=os.path.join(AUD, "am.json")),
    dict(key="nfm", row="NFM", rate=24000, chan=1, pat="480",
         rate_col="24 kHz audio", src=os.path.join(AUD, "nfm.pcm"),
         meta=os.path.join(AUD, "nfm.json")),
    dict(key="iq384mw", row="IQ, medium wave", rate=384000, chan=2, pat=PAT_384,
         rate_col="384 kHz", src=os.path.join(AUD, "iq384mw.pcm"),
         meta=os.path.join(AUD, "iq384mw.json"), iq=True),
    dict(key="quiet-6m", row="IQ, quiet band", rate=384000, chan=2, pat=PAT_384,
         rate_col="384 kHz", wav=os.path.join(HERE, "captures", "quiet-6m"),
         iq=True),
    dict(key="20m-384", row="IQ, busy band", rate=384000, chan=2, pat=PAT_384,
         rate_col="384 kHz", wav=os.path.join(HERE, "captures", "20m-384"),
         iq=True),
    dict(key="iq12", row="IQ", rate=12000, chan=2, pat="240",
         rate_col="12 kHz", src=os.path.join(AUD, "iq12.pcm"),
         meta=os.path.join(AUD, "iq12.json"), iq=True),
    dict(key="squelch", row="squelched (all silence)", rate=12000, chan=1,
         pat="240", rate_col="12 kHz audio", src=os.path.join(AUD, "squelch.pcm"),
         meta=os.path.join(AUD, "squelch.json")),

    # The rest of the IQ rates the server offers. iq48 and iq96 had no capture
    # at all; iq192 has two from 2026-09-03 that needed no receiver time.
    dict(key="iq48", row="IQ, 20 m (iq48)", rate=48000, chan=2, pat=PAT_48,
         rate_col="48 kHz", src=os.path.join(AUD, "lad-iq48.pcm"),
         meta=os.path.join(AUD, "lad-iq48.json"), iq=True),
    dict(key="iq96", row="IQ, 20 m (iq96)", rate=96000, chan=2, pat=PAT_96,
         rate_col="96 kHz", src=os.path.join(AUD, "lad-iq96.pcm"),
         meta=os.path.join(AUD, "lad-iq96.json"), iq=True),
    dict(key="20m-192", row="IQ, 20 m (iq192)", rate=192000, chan=2,
         pat=PAT_192, rate_col="192 kHz",
         wav=os.path.join(HERE, "captures", "20m-192"), iq=True),
    dict(key="40m-192", row="IQ, 40 m (iq192)", rate=192000, chan=2,
         pat=PAT_192, rate_col="192 kHz",
         wav=os.path.join(HERE, "captures", "40m-192"), iq=True),

    # The rate ladder: all five IQ modes at 14.175 MHz, back to back, so that
    # reading down the column is a comparison of rates and not of bands.
    dict(key="lad-iq", row="ladder: iq", rate=12000, chan=2, pat=PAT_12,
         rate_col="12 kHz", src=os.path.join(AUD, "lad-iq.pcm"),
         meta=os.path.join(AUD, "lad-iq.json"), iq=True),
    dict(key="lad-iq48", row="ladder: iq48", rate=48000, chan=2, pat=PAT_48,
         rate_col="48 kHz", src=os.path.join(AUD, "lad-iq48.pcm"),
         meta=os.path.join(AUD, "lad-iq48.json"), iq=True),
    dict(key="lad-iq96", row="ladder: iq96", rate=96000, chan=2, pat=PAT_96,
         rate_col="96 kHz", src=os.path.join(AUD, "lad-iq96.pcm"),
         meta=os.path.join(AUD, "lad-iq96.json"), iq=True),
    dict(key="lad-iq192", row="ladder: iq192", rate=192000, chan=2,
         pat=PAT_192, rate_col="192 kHz", src=os.path.join(AUD, "lad-iq192.pcm"),
         meta=os.path.join(AUD, "lad-iq192.json"), iq=True),
    dict(key="lad-iq384", row="ladder: iq384", rate=384000, chan=2,
         pat=PAT_384, rate_col="384 kHz", src=os.path.join(AUD, "lad-iq384.pcm"),
         meta=os.path.join(AUD, "lad-iq384.json"), iq=True),
]


def orig(key, row, rate, chan, rate_col, name, iq=False):
    """One of the nine original captures, addressed by its testdata name."""
    d = dict(key=key, row=row, rate=rate, chan=chan, pat="240",
             rate_col=rate_col, src=os.path.join(ORIG, name + ".pcm"),
             counts=os.path.join(ORIG, name + ".counts"), original=name)
    if iq:
        d["iq"] = True
    return d


ORIGINALS = [
    orig("o-cw", "CW", 12000, 1, "12 kHz audio", "cw-14025"),
    orig("o-lsb", "SSB voice", 12000, 1, "12 kHz audio", "lsb-voice-7150"),
    orig("o-usb", "SSB data", 12000, 1, "12 kHz audio", "usb-ft8-14074"),
    orig("o-am", "AM", 24000, 1, "24 kHz audio", "am-14074"),
    orig("o-nfm", "NFM", 24000, 1, "24 kHz audio", "nfm-14074"),
    orig("o-iq384-mw", "IQ, medium wave", 384000, 2, "384 kHz",
         "iq384-mw-carriers", iq=True),
    orig("o-iq384-quiet", "IQ, quiet band", 384000, 2, "384 kHz",
         "iq384-quiet-band", iq=True),
    orig("o-iq384-busy", "IQ, busy band", 384000, 2, "384 kHz",
         "iq384-ft8-14074", iq=True),
    orig("o-iq12", "IQ", 12000, 2, "12 kHz", "iq12k-ft8-14074", iq=True),
]
STREAMS = ORIGINALS + STREAMS


def pattern_frames(pat):
    out = []
    for part in pat.split(","):
        n, _, reps = part.partition("x")
        out += [int(n)] * (int(reps) if reps else 1)
    return out


def bounds_for(n_samples, frames, channels, exact=False):
    """Packet (start, end) sample offsets, the way the driver cuts them.

    `frames` repeats unless `exact`, in which case it is the capture's own list
    of packet lengths and the run ends when it does.
    """
    b, pos, i = [], 0, 0
    while True:
        if exact:
            if i >= len(frames):
                break
            n = frames[i] * channels
        else:
            n = frames[i % len(frames)] * channels
        if pos + n > n_samples:
            break
        b.append((pos, pos + n))
        pos += n
        i += 1
    return b


def run_driver(tag, src, rate, chan, pat, shifts=None, fixed=False, out=None,
               counts=None):
    cmd = [DRIVER, "-in", src, "-rate", str(rate), "-channels", str(chan),
           "-pattern", pat, "-json", os.path.join(WORK, tag + ".json")]
    if counts:
        cmd += ["-counts", counts]
    if shifts:
        cmd += ["-shifts", shifts]
    if fixed:
        cmd.append("-fixed")
    if out:
        cmd += ["-out", out]
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit(f"{tag}: {r.stderr.strip()}")
    print("   " + r.stdout.strip())
    with open(os.path.join(WORK, tag + ".json")) as f:
        return json.load(f)


def zstd_packets(x, bounds, level=3):
    """zstd over the same packets, on the samples alone.

    Level 3 is zstd's default, which is what the receiver's own PCM path used
    before this codec replaced it (klauspost SpeedDefault). Compressing per
    packet rather than over the whole file is the comparison that matters: a
    streaming codec cannot hold a window across packets it has already sent.
    """
    c = zstandard.ZstdCompressor(level=level)
    b = x.tobytes()
    return sum(len(c.compress(b[2 * a:2 * e])) for a, e in bounds)


def main():
    os.makedirs(WORK, exist_ok=True)
    subprocess.run(["go", "build", "-o", DRIVER, "."],
                   cwd=os.path.join(HERE, "spec10"), check=True)

    # With no arguments every stream is measured. Naming keys measures those
    # alone and merges them into whatever results.json already holds, so a rate
    # added later does not mean recapturing and remeasuring the rest.
    only = set(sys.argv[1:])
    out = os.path.join(WORK, "results.json")
    results = []
    if only and os.path.exists(out):
        results = [r for r in json.load(open(out)) if r["key"] not in only]
    for st in STREAMS:
        if only and st["key"] not in only:
            continue
        print(f"\n=== {st['key']}")
        if "wav" in st:
            path = sorted(glob.glob(os.path.join(st["wav"], "*.wav")))[-1]
            x, wrate = wav_samples(path)
            if wrate != st["rate"]:
                raise SystemExit(f"{st['key']}: capture is {wrate} Hz")
            src = os.path.join(WORK, st["key"] + ".pcm")
            x.tofile(src)
            st["capture_file"] = os.path.basename(path)
        else:
            src = st["src"]
            x = pcm_samples(src)
            st["capture_file"] = os.path.basename(src)
        exact = "counts" in st
        frames = ([int(v) for v in open(st["counts"]).read().split()] if exact
                  else pattern_frames(st["pat"]))
        bounds = bounds_for(len(x), frames, st["chan"], exact=exact)
        n_used = bounds[-1][1]
        print(f"   {len(x)} samples, {len(bounds)} packets, {n_used} used")

        r = dict(st)
        r.pop("src", None)
        r["arms"] = {}
        r["packets"] = len(bounds)
        cnt = st.get("counts")
        r["lossless"] = run_driver(st["key"] + ".lossless", src, st["rate"],
                                   st["chan"], st["pat"], counts=cnt)
        r["fixed"] = run_driver(st["key"] + ".fixed", src, st["rate"],
                                st["chan"], st["pat"], fixed=True, counts=cnt)
        r["zstd_bytes"] = zstd_packets(x, bounds)

        if st.get("iq"):
            xs = x[:n_used]
            for m in MARGINS:
                _, sh, depth = mr.quantise_margin(
                    x, 0, float(m), st["rate"], max_depth=MAX_DEPTH,
                    bounds=bounds)
                sp = os.path.join(WORK, f"{st['key']}.m{m}.shifts")
                with open(sp, "w") as f:
                    f.write("\n".join(str(int(v)) for v in sh))
                op = os.path.join(WORK, f"{st['key']}.m{m}.out.pcm")
                a = run_driver(f"{st['key']}.m{m}", src, st["rate"], st["chan"],
                               st["pat"], shifts=sp, out=op, counts=cnt)
                restored = np.fromfile(op, dtype="<i2")
                a["achieved_margin_db"] = float(rq.spectral_margin(xs, restored))
                a["time_snr_db"] = float(rq.time_snr(xs, restored))
                a["depth_min"] = int(depth.min())
                a["depth_max"] = int(depth.max())
                a["depth_mean"] = float(depth.mean())
                print(f"      margin {a['achieved_margin_db']:+.1f} dB "
                      f"(asked {m}), SNR {a['time_snr_db']:.1f} dB, "
                      f"shift {a['shift_min']}-{a['shift_max']} "
                      f"mean {a['shift_mean']:.2f}, depth {a['depth_min']}-"
                      f"{a['depth_max']}")
                r["arms"][f"m{m}"] = a
                os.remove(op)
        if "wav" in st:
            os.remove(src)
        if "meta" in st:
            with open(st["meta"]) as f:
                r["capture_meta"] = json.load(f)
        results.append(r)

    with open(out, "w") as f:
        json.dump(results, f, indent=1)
    print(f"\nwrote {out}")


if __name__ == "__main__":
    sys.exit(main())
