#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Reduced-depth front ends for UberSDR-IQ, for complex baseband.

The IQ counterpart of ../bitdepth/requant.py. Three things differ, and each of
them is a correction rather than a port:

- **The estimator is complex.** requant.py FFTs the sample array directly,
  which on an interleaved I/Q stream transforms I,Q,I,Q as if it were a real
  time series. That is meaningless. Here I+jQ is formed first, so the
  periodogram is the one the receiver's own noise figure describes.

- **Everything clips.** requant.py's `quantise_bfp` clips and its `quantise_snr`
  and `quantise_cap` do not. Rounding can carry a packet's peak one step past
  the ceiling -- 32767 at a shift of 6 becomes 32768 -- which wraps to -32768 on
  restore, turning a full-scale positive sample into a full-scale negative one.

- **The noise term is bounded below.** `s = max(s_cap, s_noise)` bounds the
  bitrate but not the damage: nothing stops s_noise from running far past what
  the band can afford, and on a fully occupied band it does. See `quantise_cap`.

One shift per packet covers both I and Q. That is what the header would carry,
and applying the same scale to both is a real gain, which leaves the complex
structure the predictor's single tap is there to cancel intact.
"""

import numpy as np


def as_complex(x):
    """Interleaved int16 I,Q,I,Q to complex128."""
    return x[0::2].astype(np.float64) + 1j * x[1::2].astype(np.float64)


def quantise_bfp(x, spp, bits):
    """Per-packet block floating point: the smallest shift fitting the peak."""
    lo, hi = -(1 << (bits - 1)), (1 << (bits - 1)) - 1
    q = np.empty_like(x)
    nblk = (len(x) + spp - 1) // spp
    shifts = np.zeros(nblk, dtype=np.uint8)
    for i in range(nblk):
        a, b = i * spp, min((i + 1) * spp, len(x))
        blk = x[a:b].astype(np.int32)
        peak = int(np.max(np.abs(blk)))
        s = max(0, peak.bit_length() - (bits - 1))
        shifts[i] = s
        q[a:b] = (blk if s == 0
                  else np.clip((blk + (1 << (s - 1))) >> s, lo, hi)).astype(np.int16)
    return q, shifts


def noise_floor(x, spp, rate, update_ms=100.0, window_ms=1000.0, nfft=1024):
    """Per-packet noise power of the band, in sample units.

    Stands in for the pair the header already carries. A receiver measures its
    own; without the recorder logging them this is recomputed from the samples,
    which is the one thing this experiment cannot claim to have validated.

    The floor is the median bin, corrected for the fact that a periodogram
    averaged over `nseg` segments is chi-squared with 2*nseg degrees of freedom
    rather than exponential -- its median sits close to its mean, not at 0.69 of
    it. Getting this wrong scales every noise estimate by a constant, which
    lands directly on the achieved margin.

    A lower percentile was tried, on the theory that a band with more signal
    than noise in it defeats the median. It does not help: on medium wave every
    percentile from 50 down to 2 gives the same answer, because the failure
    there is not a biased estimate but a handful of packets with a wildly wrong
    one. See `quantise_cap`.
    """
    c = as_complex(x)
    fpp = spp // 2
    win = max(int(rate * window_ms / 1000.0), fpp)
    step = max(int(rate * update_ms / 1000.0), 1)
    w = np.hanning(nfft)
    cg = (w ** 2).sum()

    nblk = (len(c) + fpp - 1) // fpp
    n0 = np.empty(nblk)
    last, nxt = None, 0
    for i in range(nblk):
        end = (i + 1) * fpp
        if last is None or end >= nxt:
            seg = c[max(0, end - win):end]
            if len(seg) >= nfft:
                nseg = len(seg) // nfft
                f = seg[:nseg * nfft].reshape(nseg, nfft) * w
                psd = (np.abs(np.fft.fft(f, axis=1)) ** 2).mean(axis=0) / cg
                # Wilson-Hilferty: median(chi2_k) ~= k(1 - 2/(9k))**3, so the
                # median of the averaged periodogram runs this far below its
                # mean. k = 2*nseg.
                k = 2.0 * nseg
                last = float(np.median(psd)) / (1.0 - 2.0 / (9.0 * k)) ** 3
                nxt = end + step
            else:
                # See margin_rule: latching a placeholder for a whole update
                # period is what made medium wave look unsalvageable.
                last = None
        n0[i] = last if last is not None else 0.0
    return n0


def shift_for_margin(n0_packet, margin_db):
    """Shift holding the quantisation floor `margin_db` under the noise floor.

    Quantising I and Q each with a step of 2**s puts 2 * (2**s)**2 / 12 of noise
    on the complex sample, so the floor is (2**s)**2 / 6.
    """
    target = n0_packet / (10.0 ** (margin_db / 10.0))
    return int(np.floor(0.5 * np.log2(max(target * 6.0, 1e-12))))


def quantise_cap(x, spp, bits, margin_db, rate, delta=None, slew=None):
    """A depth ceiling, with the noise floor used to go coarser where it can.

        s = clamp(max(s_cap, s_noise), s_cap, s_cap + delta)

    `s_cap` fits the packet's peak into `bits`, so the ceiling bounds the
    bitrate and a client can budget for the stream. `s_noise` holds the
    quantisation floor `margin_db` under the measured noise, and being the
    coarser of the two on a noisy band is what saves the bytes.

    Taking the plain max bounds what the stream costs but puts no bound at all
    on what it discards, and measured on live medium wave that is not a
    theoretical worry: 0.34% of packets took a shift of 11 where their
    neighbours took 4, and those packets carried 97.7% of all the error power
    while moving the mean shift from 4.095 to 4.118. They destroyed 12.5 dB of
    margin and bought nothing.

    Two guards are offered against that. `delta` caps how far past the ceiling
    the noise term may push, which bounds the damage but also binds constantly
    on an ordinary band, where the noise floor is 40 dB or more below the
    ceiling and the cap becomes the only thing setting the depth. `slew` limits
    how far the shift may move between adjacent packets, which is the better
    match to the failure: a genuine change in the band walks there over a few
    packets -- 5 packets is 9 ms at 192 kHz -- while an isolated spike is
    rejected outright. Both are sender-side; the packet carries a shift, not a
    policy.
    """
    n0 = noise_floor(x, spp, rate)
    prev = None
    q = np.empty_like(x)
    nblk = (len(x) + spp - 1) // spp
    shifts = np.zeros(nblk, dtype=np.uint8)
    lo, hi = -(1 << (bits - 1)), (1 << (bits - 1)) - 1
    for i in range(nblk):
        a, b = i * spp, min((i + 1) * spp, len(x))
        blk = x[a:b].astype(np.int32)
        peak = int(np.max(np.abs(blk)))
        s_cap = max(0, peak.bit_length() - (bits - 1))
        s = max(s_cap, min(shift_for_margin(n0[i], margin_db), 15))
        if delta is not None:
            s = min(s, s_cap + delta)
        if slew is not None and prev is not None:
            s = max(prev - slew, min(prev + slew, s))
        # Never coarser than leaves the peak representable at all.
        s = max(0, min(s, max(0, peak.bit_length() - 1))) if peak else 0
        shifts[i] = s
        prev = s
        q[a:b] = (blk if s == 0
                  else np.clip((blk + (1 << (s - 1))) >> s, lo, hi)).astype(np.int16)
    return q, shifts


def restore(q, shifts, spp):
    """Undo the per-packet shift, back onto the int16 grid."""
    out = np.empty_like(q)
    for i in range(len(shifts)):
        a, b = i * spp, min((i + 1) * spp, len(q))
        if a >= len(q):
            break
        out[a:b] = np.clip(q[a:b].astype(np.int32) << int(shifts[i]),
                           -32768, 32767).astype(np.int16)
    return out


def spectral_margin(x, r, nfft=4096, pct=50.0):
    """How far the quantisation floor sits below the band's own noise floor.

    The figure this experiment is scored on. Positive is headroom: at +20 dB the
    error a client sees is 20 dB under the noise it was already receiving, which
    is a promise that holds whatever the client does downstream -- unlike a bit
    depth, which says nothing without knowing the signal.
    """
    xf = x.astype(np.float64)
    e = r.astype(np.float64) - xf
    c, ce = as_complex(xf), as_complex(e)
    nseg = len(c) // nfft
    if nseg < 1:
        return float("nan")
    w = np.hanning(nfft)

    def floor(z):
        f = z[:nseg * nfft].reshape(nseg, nfft) * w
        p = (np.abs(np.fft.fft(f, axis=1)) ** 2).mean(axis=0)
        return 10 * np.log10(np.percentile(p, pct))

    return floor(c) - floor(ce)


def time_snr(x, r):
    xf = x.astype(np.float64)
    e = r.astype(np.float64) - xf
    se = float((e ** 2).sum())
    return 10 * np.log10(float((xf ** 2).sum()) / se) if se else float("inf")
