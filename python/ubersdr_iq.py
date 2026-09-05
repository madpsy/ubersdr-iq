# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""UberSDR-IQ: lossless compression for SDR sample streams.

A complete reference implementation in pure Python -- encoder and decoder, no
dependencies. It is written to be read alongside SPEC.md rather than to be
fast; see "Cost" at the end of this file for what that means in practice.

The format predicts each 16-bit sample from the ones before it with an adaptive
filter and Rice-codes the prediction error. The filter is BACKWARD adaptive:
its taps are derived from samples that have already been coded, so the decoder
recomputes them independently and nothing about them is transmitted.

That is what makes every arithmetic detail below load-bearing. The two ends
never exchange filter state; they agree only by computing the same thing. The
rounding of the prediction sum, the tap leakage, the zero-error skip, the sign
convention, the tap clamp, the order in which cascade stages are applied and
the point at which the fast path skips the clamp must all match exactly. A
difference in any of them does not fail loudly -- it returns plausible-sounding
noise.

Two of those are Python traps specifically. Python's ``>>`` and ``//`` FLOOR
towards negative infinity where this format truncates towards zero, so both
_round_shift and _leak divide the magnitude and restore the sign rather than
shifting a signed value; a naive shift is wrong for every negative input, and
negative taps are the common case. And Python's integers do not wrap where the
reference implementation's int64 would -- though none of these values ever
reaches that point, since taps are clamped to +/-2**24 and a prediction sum of
at most 16 taps against 16-bit samples stays far below 2**63. The one place a
value is deliberately narrowed is the reconstructed sample, which is written as
an int16; _to_int16 does that explicitly rather than leaving it to Python's
unbounded int.

Three profiles are defined (SPEC.md section 6.1). Profiles 0 and 1 are
lossless. Profile 2 is profile 0's predictor over samples requantised by a
power of two, for an IQ client that does not need the bottom bits; it is the
one profile whose output is not the samples that went in, and StreamEncoder
emits it only when asked, through encode_scaled.

A codec instance IS a stream. It carries the adaptation of every sample seen so
far, so create one per stream and drop it when the stream ends. Every packet
must pass through it, in order, from the first: a packet that is skipped leaves
this side's filters where the other end's no longer are.
"""

from __future__ import annotations

import struct
from typing import Iterator, List, NamedTuple, Optional, Sequence, Tuple

__all__ = [
    'MAGIC', 'Header', 'Error',
    'PROFILE_IQ', 'PROFILE_AUDIO', 'PROFILE_IQ_SCALED', 'MAX_SHIFT',
    'profile_for_channels', 'quantise', 'restore',
    'Codec', 'HeaderEncoder', 'HeaderDecoder', 'StreamEncoder', 'StreamDecoder',
    'read_stream', 'write_stream', 'quality_from_float', 'quality_to_float',
]


class Error(Exception):
    """A packet could not be encoded or decoded."""


# ---------------------------------------------------------------------------
# Wire constants
# ---------------------------------------------------------------------------

#: Identifies a packet header. Little-endian on the wire it reads "PCM4", which
#: makes it findable in a hex dump; the string is retained from the format's
#: original name and carries no meaning of its own. Four bytes and not one or
#: two because these packets may share a transport with frames that carry no
#: magic, and the width of a magic is a false-positive rate.
MAGIC = 0x344D4350

FLAG_ESCAPE = 1 << 7
FLAG_QUALITY = 1 << 6
FLAG_METADATA = 1 << 5
FLAG_SILENT = 1 << 4
FLAG_COUNT = 1 << 3
PROFILE_MASK = 0x07

#: The codepoint for "no reading". It stands in for the -999 sentinel used at
#: the API level, which cannot be represented in centidecibels because -99900
#: overflows an int16.
QUALITY_NO_READING = -32768

_QUALITY_MIN_DB = -327.67
_QUALITY_MAX_DB = 327.67

#: How often metadata is retransmitted regardless of change, in nanoseconds.
DEFAULT_RESYNC_NANOS = 5_000_000_000


def quality_from_float(v: float) -> int:
    """A dB reading as the signed centidecibels the header carries.

    The -999 sentinel, and any non-finite value, become QUALITY_NO_READING. NaN
    takes that path because a NaN comparison is false, which is intended: a
    reading that is not a number is not a reading.
    """
    if not v > -998:  # also catches NaN and -inf
        return QUALITY_NO_READING
    if v < _QUALITY_MIN_DB:
        v = _QUALITY_MIN_DB
    elif v > _QUALITY_MAX_DB:
        v = _QUALITY_MAX_DB
    # Round half away from zero, matching the reference implementation.
    x = v * 100
    return int(x + 0.5) if x >= 0 else -int(-x + 0.5)


def quality_to_float(q: int) -> float:
    """The inverse, returning -999 for "no reading"."""
    if q == QUALITY_NO_READING:
        return -999.0
    return q / 100.0


# ---------------------------------------------------------------------------
# Header
# ---------------------------------------------------------------------------

class Header(NamedTuple):
    """One packet's metadata, in the terms callers use.

    Which fields were actually transmitted is the header coder's business.
    Every field here is filled in on every packet, carried forward from the
    last resynchronisation point when the packet did not repeat it.
    """

    #: Time of the first sample in the packet, nanoseconds since the epoch.
    timestamp_nanos: int
    sample_rate: int
    #: 1 for demodulated audio, 2 for interleaved I/Q.
    channels: int
    #: int16 samples in the body, counting both channels of an IQ frame. A
    #: coded body has no length-to-count relationship, so this is what tells the
    #: decoder when to stop.
    sample_count: int
    #: dBFS, or -999 for "no reading". Passengers: nothing in the codec reads
    #: them.
    baseband_power: float
    noise: float
    #: The payload codec profile the body was coded with.
    profile: int
    #: The body holds verbatim little-endian int16 samples.
    escape: bool
    #: Every sample is zero and no body was transmitted. Mutually exclusive
    #: with escape.
    silent: bool


def _put_uvarint(out: bytearray, x: int) -> None:
    """Unsigned varint, seven bits per byte, low group first."""
    while x >= 0x80:
        out.append((x & 0x7F) | 0x80)
        x >>= 7
    out.append(x)


def _put_varint(out: bytearray, x: int) -> None:
    """Signed varint: zigzagged, then written unsigned."""
    _put_uvarint(out, (x << 1) ^ (x >> 63) if x < 0 else x << 1)


def _read_uvarint(buf: bytes, off: int) -> Tuple[int, int]:
    result = 0
    shift = 0
    for _ in range(10):
        if off >= len(buf):
            raise Error('truncated varint')
        b = buf[off]
        off += 1
        if b < 0x80:
            if shift == 63 and b > 1:
                raise Error('varint overflows 64 bits')
            return result | (b << shift), off
        result |= (b & 0x7F) << shift
        shift += 7
    raise Error('varint overflows 64 bits')


def _read_varint(buf: bytes, off: int) -> Tuple[int, int]:
    u, off = _read_uvarint(buf, off)
    return (u >> 1) ^ -(u & 1), off


class HeaderEncoder:
    """Writes headers for one stream.

    Stateful: it tracks what the peer has already been told, and transmits only
    what has changed. Measured on live traffic this averages 7.7 bytes on IQ and
    9.1 on audio, against the 37 a fixed record would cost.
    """

    def __init__(self, resync_nanos: int = DEFAULT_RESYNC_NANOS) -> None:
        self.resync_nanos = resync_nanos
        self._started = False
        self._last_ts = 0
        self._rate = 0
        self._channels = 0
        self._power = 0
        self._noise = 0
        self._count = 0
        self._last_resync = 0

    def _needs_resync(self, h: Header) -> bool:
        if not self._started or h.sample_rate != self._rate or h.channels != self._channels:
            return True
        # A timestamp that moves backwards, or jumps further than the resync
        # interval, means the stream is not continuous where a delta would
        # assume it was.
        if h.timestamp_nanos < self._last_ts:
            return True
        if h.timestamp_nanos - self._last_ts > self.resync_nanos:
            return True
        return (h.timestamp_nanos >= self._last_resync
                and h.timestamp_nanos - self._last_resync >= self.resync_nanos)

    def append(self, h: Header) -> bytes:
        power = quality_from_float(h.baseband_power)
        noise = quality_from_float(h.noise)
        resync = self._needs_resync(h)

        flags = h.profile & PROFILE_MASK
        if h.escape:
            flags |= FLAG_ESCAPE
        if h.silent:
            flags |= FLAG_SILENT
        if resync:
            flags |= FLAG_METADATA
        # Quality rides every resynchronisation so such a packet is completely
        # self-describing, and otherwise only when it has changed.
        send_quality = resync or power != self._power or noise != self._noise
        if send_quality:
            flags |= FLAG_QUALITY
        send_count = resync or h.sample_count != self._count
        if send_count:
            flags |= FLAG_COUNT

        out = bytearray(struct.pack('<IB', MAGIC, flags))
        if resync:
            out += struct.pack('<Q', h.timestamp_nanos)
        else:
            _put_varint(out, h.timestamp_nanos - self._last_ts)
        if send_count:
            _put_uvarint(out, h.sample_count)
        if resync:
            _put_uvarint(out, h.sample_rate)
            out.append(h.channels)
        if send_quality:
            out += struct.pack('<hh', power, noise)

        self._started = True
        self._last_ts = h.timestamp_nanos
        self._rate = h.sample_rate
        self._channels = h.channels
        self._power = power
        self._noise = noise
        self._count = h.sample_count
        if resync:
            self._last_resync = h.timestamp_nanos
        return bytes(out)


class HeaderDecoder:
    """Reads headers for one stream, carrying forward what is not repeated."""

    def __init__(self) -> None:
        self._have_metadata = False
        self._last_ts = 0
        self._rate = 0
        self._channels = 0
        self._count = 0
        self._power = QUALITY_NO_READING
        self._noise = QUALITY_NO_READING

    def decode(self, pkt: bytes) -> Tuple[Header, int]:
        """Parse the header at the front of pkt.

        Returns the header and the offset at which the body begins. A packet
        arriving before any metadata has been seen is rejected rather than
        guessed at; that is the normal state for a reader that opened a stored
        stream at an arbitrary offset, and the periodic resynchronisation ends
        it.
        """
        if len(pkt) < 5:
            raise Error('header: packet too short (%d bytes)' % len(pkt))
        magic = struct.unpack_from('<I', pkt, 0)[0]
        if magic != MAGIC:
            raise Error('header: bad magic 0x%08x' % magic)

        flags = pkt[4]
        off = 5
        profile = flags & PROFILE_MASK
        escape = bool(flags & FLAG_ESCAPE)
        silent = bool(flags & FLAG_SILENT)
        if escape and silent:
            raise Error('header: escape and silent are mutually exclusive')

        if flags & FLAG_METADATA:
            if len(pkt) < off + 8:
                raise Error('header: truncated timestamp')
            self._last_ts = struct.unpack_from('<Q', pkt, off)[0]
            off += 8
        else:
            if not self._have_metadata:
                raise Error('header: delta packet before any resynchronisation point')
            delta, off = _read_varint(pkt, off)
            self._last_ts += delta

        if flags & FLAG_COUNT:
            self._count, off = _read_uvarint(pkt, off)

        if flags & FLAG_METADATA:
            self._rate, off = _read_uvarint(pkt, off)
            if len(pkt) < off + 1:
                raise Error('header: truncated channel count')
            self._channels = pkt[off]
            off += 1
            self._have_metadata = True
        elif not self._have_metadata:
            raise Error('header: payload before any metadata')

        if flags & FLAG_QUALITY:
            if len(pkt) < off + 4:
                raise Error('header: truncated signal quality')
            self._power, self._noise = struct.unpack_from('<hh', pkt, off)
            off += 4

        if self._rate <= 0 or self._channels <= 0:
            raise Error('header: implausible metadata (rate %d, channels %d)'
                        % (self._rate, self._channels))
        if self._count <= 0:
            raise Error('header: implausible sample count %d' % self._count)

        return Header(
            timestamp_nanos=self._last_ts,
            sample_rate=self._rate,
            channels=self._channels,
            sample_count=self._count,
            baseband_power=quality_to_float(self._power),
            noise=quality_to_float(self._noise),
            profile=profile,
            escape=escape,
            silent=silent,
        ), off


def is_packet(pkt: bytes) -> bool:
    """True when a binary frame carries an UberSDR-IQ header."""
    return len(pkt) >= 4 and struct.unpack_from('<I', pkt, 0)[0] == MAGIC


# ---------------------------------------------------------------------------
# Predictor profiles
# ---------------------------------------------------------------------------

#: A single complex filter of order 16, for interleaved I/Q. A carrier in
#: complex baseband is a single complex pole that one complex tap cancels.
PROFILE_IQ = 0

#: A four-stage real cascade, orders 8/8/4/2, for demodulated mono audio. Depth
#: matters far more than filter length there: an oversampled channel has
#: structure at several scales for successive stages to remove.
PROFILE_AUDIO = 1

#: PROFILE_IQ's predictor, unchanged, over requantised samples: the body carries
#: a leading shift byte and the decoded samples are scaled back up by it on the
#: way out. It is a separate profile id rather than a flag bit precisely so that
#: a decoder which never asked for a reduced depth cannot be handed one by
#: accident -- an unknown profile is a hard error everywhere, where an
#: unrecognised flag might simply be ignored and the stream played 6*shift dB
#: low. See SPEC.md section 6.6.
PROFILE_IQ_SCALED = 2

#: The largest shift a scaled body may carry. Sixteen would leave nothing of an
#: int16, so anything above this is a corrupt or hostile packet.
MAX_SHIFT = 15

#: (complex, orders, mus, scaled). These ids are part of the wire format and are
#: never reassigned.
PROFILES = {
    PROFILE_IQ: (True, (16,), (16,), False),
    PROFILE_AUDIO: (False, (8, 8, 4, 2), (16, 16, 32, 32), False),
    PROFILE_IQ_SCALED: (True, (16,), (16,), True),
}

PROFILE_NAMES = {
    PROFILE_IQ: 'iq-complex-o16',
    PROFILE_AUDIO: 'audio-real-8/8/4/2',
    PROFILE_IQ_SCALED: 'iq-complex-o16-scaled',
}


def profile_for_channels(channels: int) -> int:
    """The sender's default policy. Nothing on the wire depends on it: the
    packet declares the result, so a sender may replace this freely.

    It never returns PROFILE_IQ_SCALED: a reduced depth is not something a
    channel count implies, it is something a client asks for, and a sender that
    has been asked calls StreamEncoder.encode_scaled instead.
    """
    return PROFILE_IQ if channels >= 2 else PROFILE_AUDIO


def quantise(samples: Sequence[int], shift: int) -> List[int]:
    """Requantise samples by a power of two, rounding to nearest and clamping to
    the depth the shift implies. The sender's half of PROFILE_IQ_SCALED; a
    decoder never performs it.

    Rounding rather than truncating matters: at ten bits half a step of the
    coarse grid is not a rounding detail, it is 42 dB below the peak. Clamping
    matters because rounding can carry a full-scale peak one step past the
    ceiling, and restoring that would wrap it to the opposite rail.
    """
    if shift == 0:
        return list(samples)
    half = 1 << (shift - 1)
    lo, hi = (-32768) >> shift, 32767 >> shift
    out = []
    for v in samples:
        q = (v + half) >> shift
        out.append(lo if q < lo else (hi if q > hi else q))
    return out


def restore(samples: List[int], shift: int) -> None:
    """The decoder's half: put the samples back onto the int16 grid, in place.

    The saturation is not decoration. A value at the top of the quantised range
    shifts to exactly 32767 or -32768; shifting without saturating could produce
    a value outside int16 and wrap it to the opposite rail, turning a strong
    signal into a loud artefact. Both ends must agree on it, so it is specified.
    """
    if shift == 0:
        return
    for i, v in enumerate(samples):
        r = v << shift
        samples[i] = 32767 if r > 32767 else (-32768 if r < -32768 else r)


# ---------------------------------------------------------------------------
# Adaptive filter stages
# ---------------------------------------------------------------------------

#: Fixed-point scale of the taps: integers in Q16, so 65536 is 1.0.
_TAP_SHIFT = 16
_TAP_ROUND = 1 << (_TAP_SHIFT - 1)

#: Bounds |tap| to 2**24, a real magnitude of 256. Adaptation settles around
#: 2**16, so this never fires in practice -- but it must be applied identically
#: on both sides, because if it ever does fire the two must agree. It also keeps
#: every product inside the range where a float64 represents integers exactly,
#: which is what lets a JavaScript port work without BigInt.
_TAP_LIMIT = 1 << 24

#: Leakage shifts: every tap update subtracts w/2**shift from the tap alongside
#: the gradient step. 14 for a complex stage, 16 for a real one -- see SPEC.md
#: section 6.2.3 for the measurements behind both, and for why the leak is not
#: optional. Without it sign-sign LMS has no restoring force at all: in any
#: direction the input does not excite the taps are free to walk, and on a
#: carrier-dense band they do, until the prediction is worthless and the
#: "compressed" stream is larger than the samples going into it.
_LEAK_SHIFT_COMPLEX = 14
_LEAK_SHIFT_REAL = 16


def _round_shift(v: int) -> int:
    """Divide by 2**16, rounding to nearest and away from zero on ties.

    A plain arithmetic shift rounds negative values towards negative infinity,
    which biases the predictor. Both ends must round the same way, so this is
    the single definition every implementation copies.
    """
    if v < 0:
        return -((-v + _TAP_ROUND) >> _TAP_SHIFT)
    return (v + _TAP_ROUND) >> _TAP_SHIFT


def _leak(w: int, shift: int) -> int:
    """The amount the leakage removes from one tap: the magnitude of w divided
    by 2**shift and truncated TOWARDS ZERO, with the sign of w restored.

    Truncating rather than rounding is what keeps the leak from fighting the
    gradient at small taps: below 2**shift it returns zero, so a tap the signal
    genuinely wants at a small value stays there instead of being pulled towards
    zero and pushed back on every sample.

    Python's ``>>`` floors towards negative infinity, so ``w >> shift`` is a
    DIFFERENT value on every negative tap -- and negative taps are the common
    case. Divide the magnitude and restore the sign, exactly as here.
    """
    if w < 0:
        return -((-w) >> shift)
    return w >> shift


def _to_int16(v: int) -> int:
    """Narrow to int16, wrapping, as a C or Go conversion would."""
    return ((v + 0x8000) & 0xFFFF) - 0x8000


def _history_len(order: int) -> int:
    """Size the sliding history window.

    History is linear rather than circular so the tap loops walk a contiguous
    list with no index wrapping; the cost is periodically sliding the newest
    `order` entries back to the front, which a window several times the order
    amortises away. This is an implementation choice and changes nothing that
    is transmitted.
    """
    return max(order * 8, 64)


class _ComplexStage:
    """One adaptive complex filter, for interleaved I/Q.

    Sign-sign LMS rather than true NLMS: the update needs only the signs of the
    error and of the history, so there is no division and no normalisation, and
    it is exactly reproducible in integers. Measured compression is within a
    fraction of a percent of a floating-point NLMS of the same order.
    """

    __slots__ = ('order', 'mu', 'wr', 'wi', 'hr', 'hi', 'sr', 'si', 'idx', 'fast')

    def __init__(self, order: int, mu: int) -> None:
        n = _history_len(order)
        self.order = order
        self.mu = mu
        # Taps in Q16, oldest-first: wr[j] weighs the history sample at
        # hr[idx-order+j], so prediction and adaptation walk taps and history
        # forward together.
        self.wr = [0] * order
        self.wi = [0] * order
        self.hr = [0] * n
        self.hi = [0] * n
        # The signs of the history, kept beside it so the update loop does not
        # recompute a sign per tap per sample.
        self.sr = [0] * n
        self.si = [0] * n
        self.idx = order
        self.fast = False

    def begin_packet(self, steps: int) -> None:
        """Decide once per packet whether adapt may skip the tap clamp.

        One complex update moves a tap by at most 2*mu, so if every tap starts
        further than 2*mu*steps from the limit, no update in this packet can
        reach it and the clamp is an identity. This is the path that always runs
        in practice. Skipping it entirely and always clamping produces identical
        output; it is an optimisation, not a rule of the format.
        """
        m = 0
        for w in self.wr:
            a = -w if w < 0 else w
            if a > m:
                m = a
        for w in self.wi:
            a = -w if w < 0 else w
            if a > m:
                m = a
        self.fast = m + 2 * self.mu * steps <= _TAP_LIMIT

    def _predict(self) -> Tuple[int, int]:
        lo = self.idx - self.order
        hr, hi, wr, wi = self.hr, self.hi, self.wr, self.wi
        pr = pi = 0
        for j in range(self.order):
            k = lo + j
            br, bi = hr[k], hi[k]
            w, v = wr[j], wi[j]
            pr += w * br - v * bi
            pi += w * bi + v * br
        return _round_shift(pr), _round_shift(pi)

    def _adapt_and_push(self, er: int, ei: int, xr: int, xi: int) -> None:
        # The early return on a zero error is REQUIRED, not an optimisation, and
        # it skips the leak too. Prediction over an all-zero history is zero, so
        # the error over digital silence is zero, and this is exactly the branch
        # that FREEZES the taps across a squelched run rather than leaking them
        # towards zero: whatever the filter had learned before the silence is
        # still there when signal returns. An implementation that adds the leak
        # but runs it on a zero error parts company with the encoder on the
        # first silent packet. The pair is judged jointly: a frame with a zero I
        # and a non-zero Q still adapts and still leaks.
        if er or ei:
            lo = self.idx - self.order
            mr = self.mu * ((er > 0) - (er < 0))
            mi = self.mu * ((ei > 0) - (ei < 0))
            wr, wi, sr, si = self.wr, self.wi, self.sr, self.si
            leak_shift = _LEAK_SHIFT_COMPLEX
            if self.fast:
                for j in range(self.order):
                    k = lo + j
                    hrs = sr[k]
                    # The conjugate of the history, as the complex LMS gradient
                    # requires: here that is the negated sign of the imaginary
                    # part.
                    his = -si[k]
                    # Each tap leaks from its OWN old value -- wr from wr and wi
                    # from wi, not from the magnitude of the pair -- in the same
                    # expression as the gradient step.
                    wr[j] += mr * hrs - mi * his - _leak(wr[j], leak_shift)
                    wi[j] += mr * his + mi * hrs - _leak(wi[j], leak_shift)
            else:
                for j in range(self.order):
                    k = lo + j
                    hrs = sr[k]
                    his = -si[k]
                    # The leak comes off the old tap, before the clamp.
                    a = wr[j] + mr * hrs - mi * his - _leak(wr[j], leak_shift)
                    b = wi[j] + mr * his + mi * hrs - _leak(wi[j], leak_shift)
                    wr[j] = _TAP_LIMIT if a > _TAP_LIMIT else (-_TAP_LIMIT if a < -_TAP_LIMIT else a)
                    wi[j] = _TAP_LIMIT if b > _TAP_LIMIT else (-_TAP_LIMIT if b < -_TAP_LIMIT else b)

        idx = self.idx
        self.hr[idx] = xr
        self.hi[idx] = xi
        self.sr[idx] = (xr > 0) - (xr < 0)
        self.si[idx] = (xi > 0) - (xi < 0)
        idx += 1
        if idx == len(self.hr):
            n = self.order
            self.hr[:n] = self.hr[idx - n:idx]
            self.hi[:n] = self.hi[idx - n:idx]
            self.sr[:n] = self.sr[idx - n:idx]
            self.si[:n] = self.si[idx - n:idx]
            idx = n
        self.idx = idx

    def forward(self, xr: int, xi: int) -> Tuple[int, int]:
        """Encoder direction: the residual for a known sample.

        The decoder needs this too, to advance the filters across a packet
        whose samples are already known -- an escape, or the implied zeros of a
        silent packet.
        """
        pr, pi = self._predict()
        er, ei = xr - pr, xi - pi
        self._adapt_and_push(er, ei, xr, xi)
        return er, ei

    def inverse(self, er: int, ei: int) -> Tuple[int, int]:
        """Decoder direction: reconstruct a sample from its residual.

        The same prediction, adaptation and history update as forward, which is
        what keeps the two sides identical.
        """
        pr, pi = self._predict()
        xr, xi = er + pr, ei + pi
        self._adapt_and_push(er, ei, xr, xi)
        return xr, xi


class _RealStage:
    """_ComplexStage with the imaginary terms removed, for mono audio."""

    __slots__ = ('order', 'mu', 'w', 'h', 's', 'idx', 'fast')

    def __init__(self, order: int, mu: int) -> None:
        n = _history_len(order)
        self.order = order
        self.mu = mu
        self.w = [0] * order
        self.h = [0] * n
        self.s = [0] * n
        self.idx = order
        self.fast = False

    def begin_packet(self, steps: int) -> None:
        """One real update moves a tap by at most mu, so the bound is mu*steps."""
        m = 0
        for w in self.w:
            a = -w if w < 0 else w
            if a > m:
                m = a
        self.fast = m + self.mu * steps <= _TAP_LIMIT

    def _predict(self) -> int:
        lo = self.idx - self.order
        h, w = self.h, self.w
        p = 0
        for j in range(self.order):
            p += w[j] * h[lo + j]
        return _round_shift(p)

    def _adapt_and_push(self, e: int, x: int) -> None:
        # The early return is required for the same reason as the complex form's:
        # it is what freezes the taps over digital silence rather than leaking
        # them towards zero.
        if e:
            lo = self.idx - self.order
            m = self.mu * ((e > 0) - (e < 0))
            w, s = self.w, self.s
            leak_shift = _LEAK_SHIFT_REAL
            if self.fast:
                for j in range(self.order):
                    w[j] += m * s[lo + j] - _leak(w[j], leak_shift)
            else:
                for j in range(self.order):
                    a = w[j] + m * s[lo + j] - _leak(w[j], leak_shift)
                    w[j] = _TAP_LIMIT if a > _TAP_LIMIT else (-_TAP_LIMIT if a < -_TAP_LIMIT else a)

        idx = self.idx
        self.h[idx] = x
        self.s[idx] = (x > 0) - (x < 0)
        idx += 1
        if idx == len(self.h):
            n = self.order
            self.h[:n] = self.h[idx - n:idx]
            self.s[:n] = self.s[idx - n:idx]
            idx = n
        self.idx = idx

    def forward(self, x: int) -> int:
        e = x - self._predict()
        self._adapt_and_push(e, x)
        return e

    def inverse(self, e: int) -> int:
        x = e + self._predict()
        self._adapt_and_push(e, x)
        return x


# ---------------------------------------------------------------------------
# Rice coding
# ---------------------------------------------------------------------------
#
# A residual is coded as its zigzagged magnitude split at bit k: the high part
# in unary, then a 0 stop bit, then the low k bits raw. k is chosen once per
# packet from the mean magnitude, which for a Laplacian source is within a
# fraction of a bit of optimal, and sent as the first byte of the body.
#
# Bits are written least-significant first within each byte, and bytes in
# increasing address order.


def _zigzag(v: int) -> int:
    return (v << 1) ^ (v >> 31) if v < 0 else v << 1


def _choose_k(res: Sequence[int]) -> int:
    total = 0
    for v in res:
        total += _zigzag(v)
    mean = total // len(res)
    if mean <= 0:
        return 0
    return min(mean.bit_length() - 1, 30)


def rice_encode(res: Sequence[int]) -> bytes:
    """Code residuals, returning the k byte followed by the bitstream."""
    k = _choose_k(res)
    out = bytearray([k])
    mask = (1 << k) - 1
    acc = 0
    nbits = 0
    for v in res:
        u = _zigzag(v)
        q = u >> k
        # The unary run, in chunks, so the accumulator stays small.
        while q > 0:
            c = 24 if q > 24 else q
            acc |= ((1 << c) - 1) << nbits
            nbits += c
            while nbits >= 8:
                out.append(acc & 0xFF)
                acc >>= 8
                nbits -= 8
            q -= c
        # The stop bit is the 0 left by not setting it, then k raw bits.
        acc |= (u & mask) << (nbits + 1)
        nbits += k + 1
        while nbits >= 8:
            out.append(acc & 0xFF)
            acc >>= 8
            nbits -= 8
    if nbits:
        out.append(acc & 0xFF)
    return bytes(out)


def rice_decode(src: bytes, count: int) -> List[int]:
    """Reverse rice_encode."""
    if len(src) < 1:
        raise Error('rice: empty bitstream')
    k = src[0]
    if k > 30:
        raise Error('rice: invalid k %d' % k)

    # The whole body as one integer, least-significant byte first, so the bit
    # cursor is a shift rather than a refill loop.
    body = src[1:]
    acc = int.from_bytes(body, 'little')
    total_bits = len(body) * 8
    mask = (1 << k) - 1

    out = [0] * count
    pos = 0
    for j in range(count):
        q = 0
        while True:
            if pos >= total_bits:
                raise Error('rice: truncated at value %d' % j)
            if not (acc >> pos) & 1:
                pos += 1
                break
            q += 1
            pos += 1
        if pos + k > total_bits:
            raise Error('rice: truncated remainder at value %d' % j)
        u = (q << k) | ((acc >> pos) & mask)
        pos += k
        out[j] = (u >> 1) ^ -(u & 1)
    return out


# ---------------------------------------------------------------------------
# Payload codec
# ---------------------------------------------------------------------------

class Codec:
    """Codes one direction of one stream's payloads.

    Stateful across packets: create one per stream and drop it when the stream
    ends. Its taps carry the adaptation of every sample coded so far, which is
    exactly why it cannot be shared or reused.
    """

    __slots__ = ('profile_id', 'complex', 'scaled', 'stages')

    def __init__(self, profile_id: int) -> None:
        """Build a codec for a profile id, rejecting one this build does not
        implement.

        The error is deliberate. Falling back to a default would decode with the
        wrong predictor and return plausible-sounding noise rather than failing,
        which is the worst outcome for a format whose promise is bit-exactness.
        """
        spec = PROFILES.get(profile_id)
        if spec is None:
            raise Error('unknown profile id %d' % profile_id)
        is_complex, orders, mus, scaled = spec
        self.profile_id = profile_id
        self.complex = is_complex
        # Whether the body carries a leading shift byte and the output is scaled
        # back up. It changes nothing about the predictor: the filters run on the
        # values on the wire, on both sides, exactly as for an unscaled profile.
        self.scaled = scaled
        if is_complex:
            self.stages = [_ComplexStage(o, m) for o, m in zip(orders, mus)]
        else:
            self.stages = [_RealStage(o, m) for o, m in zip(orders, mus)]

    @property
    def profile_name(self) -> str:
        return PROFILE_NAMES.get(self.profile_id, 'profile-%d' % self.profile_id)

    @property
    def samples_per_step(self) -> int:
        """2 for a complex profile, which consumes interleaved I/Q frames."""
        return 2 if self.complex else 1

    def _begin_packet(self, steps: int) -> None:
        for s in self.stages:
            s.begin_packet(steps)

    def _forward(self, a: int, b: int) -> Tuple[int, int]:
        """Run the cascade in the encoder direction over one sample position.
        Each stage predicts the residual left by the one before it."""
        if self.complex:
            for s in self.stages:
                a, b = s.forward(a, b)
            return a, b
        for s in self.stages:
            a = s.forward(a)
        return a, 0

    def encode(self, samples: Sequence[int]) -> Tuple[bytes, bool]:
        """Code one packet, returning (body, escape).

        The caller must carry the escape flag, and the profile id, in the packet
        header. For a complex profile the samples are interleaved I/Q and the
        count must be even.
        """
        n = len(samples)
        step = self.samples_per_step
        if n == 0:
            raise Error('empty packet')
        if n % step:
            raise Error('%d samples is not a whole number of %d-channel frames' % (n, step))

        self._begin_packet(n // step)
        res = [0] * n
        if step == 2:
            for i in range(0, n, 2):
                res[i], res[i + 1] = self._forward(samples[i], samples[i + 1])
        else:
            for i in range(n):
                res[i] = self._forward(samples[i], 0)[0]

        body = rice_encode(res)

        # If prediction and coding did not pay for themselves, send the samples
        # as they are. A predictor cannot help a full-entropy signal, and a
        # saturated front end produces exactly that; without this the format
        # would expand by about 3% on such a stream. The filters have already
        # adapted over this packet, and the decoder adapts over the same
        # verbatim samples, so state stays in step through an escape.
        if len(body) >= n * 2:
            return struct.pack('<%dh' % n, *samples), True
        return body, False

    def advance_silence(self, count: int) -> None:
        """Advance the filters over count zero samples without coding them.

        A packet whose samples are all zero says so in its header and carries no
        body. Rice coding cannot get an all-zero residual below one bit per
        sample -- it is a bare stop bit -- so a 240-sample packet would cost 30
        bytes of stop bits however well the predictor did. The predictor still
        has to run: the other end advances its own filters over the same zeros,
        and the two must agree sample for sample.
        """
        step = self.samples_per_step
        if count <= 0:
            raise Error('empty packet')
        if count % step:
            raise Error('%d samples is not a whole number of %d-channel frames' % (count, step))
        self._begin_packet(count // step)
        for _ in range(0, count, step):
            self._forward(0, 0)

    def decode(self, body: bytes, count: int, escape: bool) -> List[int]:
        """Reconstruct one packet body, with the escape flag from its header."""
        step = self.samples_per_step
        if count <= 0 or count % step:
            raise Error('bad sample count %d for %d-channel profile' % (count, step))

        if escape:
            if len(body) < count * 2:
                raise Error('escape body truncated (%d bytes for %d samples)' % (len(body), count))
            out = list(struct.unpack_from('<%dh' % count, body, 0))
            # Advance the filters over these samples exactly as the encoder did,
            # discarding the residuals.
            self._begin_packet(count // step)
            if step == 2:
                for i in range(0, count, 2):
                    self._forward(out[i], out[i + 1])
            else:
                for i in range(count):
                    self._forward(out[i], 0)
            return out

        res = rice_decode(body, count)
        out = [0] * count
        self._begin_packet(count // step)
        # Stages are inverted in reverse order: the last stage to have predicted
        # is the first to be undone.
        rev = list(reversed(self.stages))
        if self.complex:
            for i in range(0, count, 2):
                a, b = res[i], res[i + 1]
                for s in rev:
                    a, b = s.inverse(a, b)
                out[i] = _to_int16(a)
                out[i + 1] = _to_int16(b)
        else:
            for i in range(count):
                a = res[i]
                for s in rev:
                    a = s.inverse(a)
                out[i] = _to_int16(a)
        return out


# ---------------------------------------------------------------------------
# Packet assembly
# ---------------------------------------------------------------------------

class StreamEncoder:
    """Builds packets for one stream: the header and the payload codec together."""

    def __init__(self, resync_nanos: int = DEFAULT_RESYNC_NANOS) -> None:
        self.header = HeaderEncoder(resync_nanos)
        self._codec: Optional[Codec] = None
        self._profile = -1

    def encode(self, samples: Sequence[int], timestamp_nanos: int, sample_rate: int,
               channels: int, baseband_power: float = -999.0,
               noise: float = -999.0) -> bytes:
        """Assemble one lossless packet.

        samples are int16 values, interleaved I/Q when channels is 2.
        """
        return self._encode(samples, timestamp_nanos, sample_rate, channels,
                            baseband_power, noise, False, 0)

    def encode_scaled(self, samples: Sequence[int], timestamp_nanos: int, sample_rate: int,
                      channels: int, baseband_power: float = -999.0,
                      noise: float = -999.0, shift: int = 0) -> bytes:
        """Assemble one reduced-depth IQ packet: PROFILE_IQ_SCALED, with the
        samples requantised by shift before the predictor sees them and the
        shift itself as the first byte of the body.

        shift must be 0..MAX_SHIFT and channels must be 2 -- there is no scaled
        real profile. The caller's samples are not modified; the requantisation
        is lossy and is done on a copy.

        Which shift to use is the caller's policy and nothing on the wire depends
        on how it was chosen (SPEC.md section 6.6.4). A shift of 0 is legal and
        codes the samples exactly, but a sender that wants a lossless stream
        should use encode() and profile 0 rather than declaring a lossy profile
        and shifting by nothing.
        """
        if channels != 2:
            raise Error('the scaled profile is complex, so it needs 2 channels, not %d' % channels)
        if not 0 <= shift <= MAX_SHIFT:
            raise Error('shift %d out of range 0..%d' % (shift, MAX_SHIFT))
        return self._encode(samples, timestamp_nanos, sample_rate, channels,
                            baseband_power, noise, True, shift)

    def _encode(self, samples: Sequence[int], timestamp_nanos: int, sample_rate: int,
                channels: int, baseband_power: float, noise: float,
                scaled: bool, shift: int) -> bytes:
        if channels <= 0:
            channels = 1
        if not samples:
            raise Error('empty packet')

        # The predictor form follows the channel count, so a change rebuilds the
        # codec. Discarding the adaptation is correct rather than merely
        # acceptable: the new signal is a different one, and taps trained on the
        # old would predict it worse than a cold start. Crossing between the
        # lossless and the scaled IQ profile is the same event: the profile id on
        # the wire changes, so both ends rebuild.
        want = PROFILE_IQ_SCALED if scaled else profile_for_channels(channels)
        if self._codec is None or want != self._profile:
            self._codec = Codec(want)
            self._profile = want

        # A complex profile consumes whole I/Q frames; a truncated final frame
        # would desynchronise the predictor, so drop it.
        if self._codec.samples_per_step == 2 and len(samples) % 2:
            samples = samples[:-1]
        if not samples:
            raise Error('no whole frames in packet')

        # Requantise before anything else looks at the samples. The predictor,
        # on both sides, runs on the requantised values; the decoder undoes the
        # scale only on the way out (SPEC.md section 6.6.2). The silence scan
        # below therefore sees the quantised values too, which is what the
        # decoder's advance_silence will reproduce.
        if scaled:
            samples = quantise(samples, shift)

        # Detecting silence rather than being told about it catches every
        # source of it, and the scan costs a fraction of what coding one does.
        silent = not any(samples)
        body = b''
        escape = False
        if silent:
            self._codec.advance_silence(len(samples))
        else:
            body, escape = self._codec.encode(samples)

        h = Header(
            timestamp_nanos=timestamp_nanos,
            sample_rate=sample_rate,
            channels=channels,
            sample_count=len(samples),
            baseband_power=baseband_power,
            noise=noise,
            profile=want,
            escape=escape,
            silent=silent,
        )
        # The shift leads the body rather than riding in the header: the flags
        # byte is full, and a silent packet has no body at all, so putting it
        # here costs a squelched or dead channel nothing.
        if scaled and not silent:
            return self.header.append(h) + bytes((shift,)) + body
        return self.header.append(h) + body


class StreamDecoder:
    """Reads packets for one stream.

    Every packet must pass through here, in order, from the first. A packet that
    is skipped -- even one whose samples the caller intends to discard -- leaves
    this side's filters where the encoder's no longer are, and everything after
    it decodes to noise.
    """

    def __init__(self) -> None:
        self.header = HeaderDecoder()
        self._codec: Optional[Codec] = None

    def decode(self, pkt: bytes) -> Tuple[Header, List[int]]:
        """Return the header and the samples, interleaved I/Q for two channels."""
        h, off = self.header.decode(pkt)

        # The packet declares its own profile; nothing here infers it from the
        # channel count.
        if self._codec is None or self._codec.profile_id != h.profile:
            self._codec = Codec(h.profile)

        if h.silent:
            if len(pkt) != off:
                raise Error('silent packet carries %d bytes of body' % (len(pkt) - off))
            self._codec.advance_silence(h.sample_count)
            return h, [0] * h.sample_count

        # A scaled profile carries the shift as the first byte of the body. It
        # is read only here, after the silent case has returned: a silent packet
        # on a scaled profile carries no shift, because it carries no body.
        shift = 0
        if self._codec.scaled:
            if len(pkt) <= off:
                raise Error('scaled packet carries no shift byte')
            shift = pkt[off]
            if shift > MAX_SHIFT:
                raise Error('shift %d out of range 0..%d' % (shift, MAX_SHIFT))
            off += 1

        out = self._codec.decode(pkt[off:], h.sample_count, h.escape)
        # Undo the scale only on the way out. The predictor above ran on the
        # values that were on the wire, exactly as the encoder's did.
        restore(out, shift)
        return h, out


# ---------------------------------------------------------------------------
# Stream files
# ---------------------------------------------------------------------------
#
# The packet format says nothing about where one packet ends and the next
# begins, because on a message transport the frame boundary already does.
# Storing packets in a file needs that boundary back, so:
#
#     [length uint32 little-endian][packet bytes]  repeated to end of file
#
# There is no file header and no index. The convention is the extension .uiq.
# This container is a convenience, not part of the format.


def read_stream(f) -> Iterator[bytes]:
    """Yield packets from a length-prefixed stream file."""
    while True:
        prefix = f.read(4)
        if not prefix:
            return
        if len(prefix) != 4:
            raise Error('truncated length prefix')
        n = struct.unpack('<I', prefix)[0]
        if n <= 0 or n > (1 << 26):
            raise Error('implausible packet length %d' % n)
        pkt = f.read(n)
        if len(pkt) != n:
            raise Error('truncated packet')
        yield pkt


def write_stream(f, packets) -> None:
    """Write packets to a length-prefixed stream file."""
    for pkt in packets:
        f.write(struct.pack('<I', len(pkt)))
        f.write(pkt)


# ---------------------------------------------------------------------------
# Cost
# ---------------------------------------------------------------------------
#
# This implementation is pure Python and the predictor is strictly sequential:
# every sample depends on the ones before it and the taps adapt per sample, so
# it cannot be vectorised with numpy and cannot be handed to a C library the way
# a general-purpose compressor could. It runs at interpreter speed.
#
# Measured with example.py on this repository's vectors, CPython 3 on an x86-64
# desktop:
#
#     real cascade (audio):     147 k samples/s encode, 135 k decode
#     complex profile (IQ):      96 k frames/s encode,   87 k decode
#
# The C++ implementation in this repository is about a hundred times faster on
# the same data. What that means in practice:
#
#     12 kHz audio     needs 12 k samples/s   -- about 8% of capacity
#     12 kHz IQ        needs 12 k frames/s    -- about 12%
#     384 kHz IQ       needs 384 k frames/s   -- four times too fast
#
# So demodulated audio and narrowband IQ decode comfortably in real time and
# wideband IQ does not. Inlining the round-shift and walking the history with
# zip rather than indices measured 1.02x, so no ordinary Python optimisation
# closes that gap; it would take a C extension.
#
# This is a property of the interpreter and not a defect, and it is recorded
# here so that someone watching a high-rate stream fall behind finds the reason
# next to the code rather than having to measure it again.
