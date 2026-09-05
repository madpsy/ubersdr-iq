#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""An experimental 8-bit mode for UberSDR-IQ, and the tools to test it.

The format codes int16 samples losslessly. An 8-bit mode is a front end to
that, not a change to it: requantise each packet to 8 bits, hand those small
values to the existing predictor and Rice coder, and carry the scale in the
header so the receiving end can undo it. The body codec is untouched. The
saving comes entirely from the numbers going into it being smaller, which
shortens every Rice codeword.

The scale is a power of two, so applying and undoing it are shifts. That keeps
the path in integers, which the format requires: both ends must arrive at
bit-identical filter state, and a floating-point gain would not guarantee it.

Each packet gets the smallest shift that fits its own peak into int8, so a
quiet packet keeps more resolution than a loud one and the quantisation floor
follows the signal down. One shift per packet is what the header would carry --
a nibble, against a packet that is hundreds of bytes.

    requant.py quantise MODE in.pcm out.pcm shifts.bin [--frames N]
    requant.py restore out.pcm shifts.bin restored.pcm [--frames N]

MODE is one of:

    bfpN      per-packet block floating point to N bits, peak-driven
    snrM      depth chosen per packet from the signal quality the header
              already carries, keeping the quantisation floor M dB below the
              measured noise floor. Readings arrive every 100 ms, as the
              receiver supplies them, each smoothed over 1 s
    snrfastM  the same, with each reading computed from only the last 100 ms
    capNmM    an N-bit ceiling with the noise reading used to go coarser where
              the band allows -- bounded bandwidth, noise-aware placement
    truncN    one fixed scale for the whole stream (the naive version)

`quantise` writes the small values, which are what the codec would carry.
`restore` puts them back on the int16 grid, which is what a client would play.
Sample files are raw little-endian int16.
"""

import sys
import numpy as np

DEFAULT_FRAMES = 240


def quantise_bfp(x: np.ndarray, frames: int, bits: int):
    """Per-packet block floating point to `bits` bits."""
    lo, hi = -(1 << (bits - 1)), (1 << (bits - 1)) - 1
    keep = bits - 1  # magnitude bits
    q = np.empty_like(x)
    nblk = (len(x) + frames - 1) // frames
    shifts = np.zeros(nblk, dtype=np.uint8)
    for i in range(nblk):
        a, b = i * frames, min((i + 1) * frames, len(x))
        blk = x[a:b].astype(np.int32)
        peak = int(np.max(np.abs(blk)))
        # peak.bit_length() - keep is the smallest s with (peak >> s) <= hi.
        s = max(0, peak.bit_length() - keep)
        shifts[i] = s
        if s == 0:
            q[a:b] = blk.astype(np.int16)
            continue
        # Round to nearest rather than truncate. At eight bits half an LSB of
        # the coarse grid is not a rounding detail, it is 42 dB down on peak.
        v = (blk + (1 << (s - 1))) >> s
        q[a:b] = np.clip(v, lo, hi).astype(np.int16)
    return q, shifts


def estimate_quality(x: np.ndarray, frames: int, rate: int = 12000,
                     update_ms: float = 100.0, window_ms: float = 1000.0):
    """Stand in for the signal-quality pair the header already carries.

    SPEC.md 5.6 puts two readings in every packet: baseband power and a noise
    figure, both in centidecibels, updated when they change and carried forward
    otherwise. A receiver measures them properly; here they are recomputed from
    the samples so the experiment can use them without a live receiver.

    Noise is the median bin of a periodogram over a trailing window. The median
    is what makes it a noise floor rather than a signal measurement: on a band
    carrying fifty FT8 signals, most bins are still noise.

    `update_ms` is how often a new reading arrives -- 100 ms from the receiver
    this format is deployed against, which is five packets at 12 kHz. Between
    updates the previous reading stands, exactly as the header's carried-forward
    value does. `window_ms` is how much history each reading is computed from,
    standing in for the receiver's own smoothing.

    Returns per-packet (baseband power, noise power), both linear, both in the
    units the samples are in.
    """
    win = max(int(rate * window_ms / 1000.0), frames)
    step = max(int(rate * update_ms / 1000.0), 1)
    nfft = 2048 if win >= 2048 else 256
    w = np.hanning(nfft)
    cg = (w ** 2).sum()

    nblk = (len(x) + frames - 1) // frames
    bb = np.empty(nblk)
    n0 = np.empty(nblk)
    last_bb = last_n0 = None
    next_update = 0
    for i in range(nblk):
        a = i * frames
        end = a + frames
        if last_n0 is None or end >= next_update:
            # The window trails, as a live encoder's must: it can only measure
            # what has already arrived.
            seg = x[max(0, end - win):end].astype(np.float64)
            last_bb = float(np.mean(seg ** 2)) if len(seg) else 1.0
            if len(seg) >= nfft:
                nseg = len(seg) // nfft
                f = seg[:nseg * nfft].reshape(nseg, nfft) * w
                psd = (np.abs(np.fft.rfft(f, axis=1)) ** 2).mean(axis=0) / cg
                # Median bin scaled back to a total power across the band.
                last_n0 = float(np.median(psd)) * len(psd) * 2 / nfft
            else:
                last_n0 = last_bb
            next_update = end + step
        bb[i], n0[i] = last_bb, last_n0
    return bb, n0


def quantise_snr(x: np.ndarray, frames: int, margin_db: float, rate: int = 12000,
                 window_ms: float = 1000.0):
    """Choose each packet's depth from the signal quality the header carries.

    A fixed depth is a guess about how much dynamic range the band needs. The
    header already carries the measurement that answers it: the span a packet
    must represent runs from its peak, which must not clip, down to below the
    noise floor, which must not be raised. That span is what baseband power and
    noise describe.

    So rather than fixing the depth and letting the quantisation floor land
    where it may, fix the floor -- `margin_db` below the measured noise -- and
    let the depth come out of it. A noisy band then costs fewer bits and a quiet
    one more, which is the right way round and is what a fixed depth cannot do.
    """
    _, n0 = estimate_quality(x, frames, rate, window_ms=window_ms)
    q = np.empty_like(x)
    nblk = (len(x) + frames - 1) // frames
    shifts = np.zeros(nblk, dtype=np.uint8)
    depths = np.zeros(nblk, dtype=np.uint8)

    for i in range(nblk):
        a, b = i * frames, min((i + 1) * frames, len(x))
        blk = x[a:b].astype(np.int32)
        peak = int(np.max(np.abs(blk)))

        # Quantisation noise of a step of 2**s is (2**s)**2 / 12. Keep it
        # margin_db below the measured noise floor and solve for s.
        target = n0[i] / (10.0 ** (margin_db / 10.0))
        s = int(np.floor(0.5 * np.log2(max(target * 12.0, 1e-12))))
        s = max(0, min(s, 15))
        # Never coarser than the peak allows to be represented at all.
        s = min(s, max(0, peak.bit_length() - 1))
        shifts[i] = s
        depths[i] = max(2, (peak >> s).bit_length() + 1) if peak else 2

        if s == 0:
            q[a:b] = blk.astype(np.int16)
            continue
        v = (blk + (1 << (s - 1))) >> s
        q[a:b] = v.astype(np.int16)

    return q, shifts, depths


def quantise_cap(x: np.ndarray, frames: int, bits: int, margin_db: float,
                 rate: int = 12000, window_ms: float = 100.0):
    """A depth cap, with the noise reading used to go coarser where it can.

    This is the shape a shipped lossy mode probably wants. The cap fixes the
    ceiling, so bandwidth is bounded and a client can budget for it; within that
    ceiling the noise reading decides how much of the budget is actually needed.
    The two constraints are combined by taking whichever shift is coarser:

        s = max(s_cap, s_noise)

    `s_cap` is the shift that fits the packet's peak into `bits` bits, so the
    result can never need more than the cap. `s_noise` is the shift that holds
    the quantisation floor `margin_db` under the measured noise; when the band
    is noisy that is the coarser of the two and the packet costs less than the
    cap would have. The noise reading can therefore only ever save bytes here,
    never spend them, which is what makes the ceiling meaningful.
    """
    _, n0 = estimate_quality(x, frames, rate, window_ms=window_ms)
    q = np.empty_like(x)
    nblk = (len(x) + frames - 1) // frames
    shifts = np.zeros(nblk, dtype=np.uint8)
    depths = np.zeros(nblk, dtype=np.uint8)

    for i in range(nblk):
        a, b = i * frames, min((i + 1) * frames, len(x))
        blk = x[a:b].astype(np.int32)
        peak = int(np.max(np.abs(blk)))

        s_cap = max(0, peak.bit_length() - (bits - 1))
        target = n0[i] / (10.0 ** (margin_db / 10.0))
        s_noise = int(np.floor(0.5 * np.log2(max(target * 12.0, 1e-12))))
        s = max(s_cap, min(s_noise, 15))
        s = max(0, min(s, max(0, peak.bit_length() - 1)) if peak else 0)
        shifts[i] = s
        depths[i] = max(2, (peak >> s).bit_length() + 1) if peak else 2

        if s == 0:
            q[a:b] = blk.astype(np.int16)
            continue
        v = (blk + (1 << (s - 1))) >> s
        q[a:b] = v.astype(np.int16)

    return q, shifts, depths


def quantise_trunc(x: np.ndarray, frames: int, bits: int):
    """One fixed scale for the whole stream, chosen to fit the global peak.

    Note that this is NOT implementable as a live encoder: it reads the whole
    recording to pick its scale, so it knows the loudest sample before it emits
    the first one. It is here as an offline lower bound on what a fixed scale
    could achieve, not as a candidate.
    """
    lo, hi = -(1 << (bits - 1)), (1 << (bits - 1)) - 1
    peak = int(np.max(np.abs(x)))
    s = max(0, peak.bit_length() - (bits - 1))
    v = np.clip((x.astype(np.int32) + (1 << (s - 1) if s else 0)) >> s, lo, hi)
    nblk = (len(x) + frames - 1) // frames
    return v.astype(np.int16), np.full(nblk, s, dtype=np.uint8)


def restore(q: np.ndarray, shifts: np.ndarray, frames: int) -> np.ndarray:
    """Undo the per-packet shift, putting the samples back on the int16 grid."""
    out = np.empty_like(q)
    for i in range(len(shifts)):
        a, b = i * frames, min((i + 1) * frames, len(q))
        if a >= len(q):
            break
        out[a:b] = (q[a:b].astype(np.int32) << int(shifts[i])).astype(np.int16)
    return out


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    cmd = sys.argv[1]
    frames = DEFAULT_FRAMES
    if "--frames" in sys.argv:
        frames = int(sys.argv[sys.argv.index("--frames") + 1])

    if cmd == "quantise":
        mode, src, dst, shpath = sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
        x = np.fromfile(src, dtype="<i2")
        depths = None
        if mode.startswith("cap"):
            # capNmM -- N-bit ceiling, M dB margin under the noise floor
            n, m = mode[3:].split("m")
            q, shifts, depths = quantise_cap(x, frames, int(n), float(m))
        elif mode.startswith("bfp"):
            q, shifts = quantise_bfp(x, frames, int(mode[3:]))
        elif mode.startswith("snrfast"):
            q, shifts, depths = quantise_snr(x, frames, float(mode[7:]),
                                             window_ms=100.0)
        elif mode.startswith("snr"):
            q, shifts, depths = quantise_snr(x, frames, float(mode[3:]))
        elif mode.startswith("trunc"):
            q, shifts = quantise_trunc(x, frames, int(mode[5:]))
        else:
            print(f"unknown mode {mode!r}", file=sys.stderr)
            return 2
        q.tofile(dst)
        shifts.tofile(shpath)

        # Report what the requantisation itself cost, before the codec sees it.
        y = restore(q, shifts, frames).astype(np.float64)
        xf = x.astype(np.float64)
        err = y - xf
        se = float(np.sum(err ** 2))
        snr = 10 * np.log10(float(np.sum(xf ** 2)) / se) if se else float("inf")
        extra = ""
        if depths is not None:
            extra = (f", depth {int(depths.min())}-{int(depths.max())} "
                     f"(mean {depths.mean():.2f} bits)")
        print(f"{mode:7s} {len(x)} samples, peak {int(np.max(np.abs(x)))}, "
              f"shift {int(shifts.min())}-{int(shifts.max())}, "
              f"SNR {snr:.2f} dB, max err {int(np.max(np.abs(err)))}{extra}")
        return 0

    if cmd == "restore":
        src, shpath, dst = sys.argv[2], sys.argv[3], sys.argv[4]
        q = np.fromfile(src, dtype="<i2")
        shifts = np.fromfile(shpath, dtype=np.uint8)
        restore(q, shifts, frames).tofile(dst)
        return 0

    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main())
