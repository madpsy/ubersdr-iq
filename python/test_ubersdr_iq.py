#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Conformance and unit tests for the Python implementation.

    python3 -m unittest discover -s python

The conformance tests read testdata/ and check two things per vector:

  decoding   the .uiq stream must reconstruct the .pcm samples exactly. This is
             the correctness requirement for anything that reads the format.

  encoding   the .pcm samples must produce the .uiq stream byte for byte. The
             format does not require that -- an encoder may legitimately choose
             a different k or take the escape earlier -- but byte-identity is a
             far sharper test of a port, because it catches a predictor that is
             subtly wrong in a way its own decoder would reproduce and hide.
"""

import os
import struct
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import ubersdr_iq as uiq  # noqa: E402

TESTDATA = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'testdata')

# name, sample rate, channels, frames per packet
VECTORS = [
    ('audio-cw-12k', 12000, 1, 240),
    ('audio-usb-12k', 12000, 1, 240),
    ('iq-12k', 12000, 2, 240),
    ('iq-384k', 384000, 2, 360),
    ('iq-384k-mw', 384000, 2, 360),
    ('rice-63', 96000, 2, 240),
]

# The timestamp schedule the vectors were produced with.
START_NS = 1_700_000_000_000_000_000


def load_pcm(name):
    with open(os.path.join(TESTDATA, name + '.pcm'), 'rb') as f:
        raw = f.read()
    return list(struct.unpack('<%dh' % (len(raw) // 2), raw))


def load_packets(name):
    with open(os.path.join(TESTDATA, name + '.uiq'), 'rb') as f:
        return list(uiq.read_stream(f))


class TestConformance(unittest.TestCase):
    def test_decode_is_exact(self):
        for name, rate, channels, _ in VECTORS:
            with self.subTest(name):
                want = load_pcm(name)
                dec = uiq.StreamDecoder()
                got = []
                wire = 0
                for n, pkt in enumerate(load_packets(name)):
                    h, s = dec.decode(pkt)
                    self.assertEqual(h.sample_rate, rate, 'packet %d' % n)
                    self.assertEqual(h.channels, channels, 'packet %d' % n)
                    got.extend(s)
                    wire += len(pkt)
                self.assertEqual(len(got), len(want))
                self.assertEqual(got, want, '%s is not lossless' % name)
                print('  %-16s %6d samples, %.3fx' % (name, len(want), len(want) * 2 / wire))

    def test_encode_is_byte_identical(self):
        for name, rate, channels, frames in VECTORS:
            with self.subTest(name):
                samples = load_pcm(name)
                want = load_packets(name)
                enc = uiq.StreamEncoder()
                per = frames * channels
                n = 0
                for i in range(0, len(samples) - per + 1, per):
                    ts = START_NS + (i // channels) * 10**9 // rate
                    pkt = enc.encode(samples[i:i + per], ts, rate, channels, -999, -999)
                    self.assertLess(n, len(want), 'encoder produced too many packets')
                    self.assertEqual(pkt, want[n], '%s packet %d differs' % (name, n))
                    n += 1
                self.assertEqual(n, len(want))

    def test_edge_cases(self):
        """Silence, escapes, count and rate changes, and a profile change."""
        want = load_pcm('edge-cases')
        dec = uiq.StreamDecoder()
        got = []
        silent = escape = resync = 0
        profiles = set()
        for n, pkt in enumerate(load_packets('edge-cases')):
            if pkt[4] & uiq.FLAG_METADATA:
                resync += 1
            h, s = dec.decode(pkt)
            silent += h.silent
            escape += h.escape
            profiles.add(h.profile)
            got.extend(s)
        self.assertEqual(got, want)
        self.assertGreater(silent, 0)
        self.assertGreater(escape, 0)
        self.assertGreater(resync, 1)
        self.assertGreater(len(profiles), 1)
        print('  edge-cases       %d silent, %d escaped, %d resync, profiles %s'
              % (silent, escape, resync, sorted(profiles)))

    def test_iq_scaled(self):
        """Profile 2 (SPEC.md section 6.6): the shift byte, the silent scaled
        packet that has none, an escaped one that does, and the crossings in and
        out of the lossless profile.

        Decode-only, like edge-cases: the vector is defined by a generator in
        go/ubersdriq/vectors_test.go rather than by encoding a .pcm with fixed
        parameters, and it additionally depends on a shift schedule that is not
        derivable from its samples. Its .pcm holds the RESTORED samples -- what a
        decoder must produce, not what went into the encoder. This is the one
        vector where those differ.
        """
        want = load_pcm('iq-scaled')
        dec = uiq.StreamDecoder()
        # A second, independent header decoder, so the shift byte at the front
        # of each scaled body can be read without reaching inside the stream
        # decoder's state.
        hdr = uiq.HeaderDecoder()
        got = []
        scaled = silent = escape = changes = 0
        shifts = {}
        last = -1
        for n, pkt in enumerate(load_packets('iq-scaled')):
            hh, off = hdr.decode(pkt)
            if hh.profile == uiq.PROFILE_IQ_SCALED and not hh.silent:
                shifts[pkt[off]] = shifts.get(pkt[off], 0) + 1
            h, s = dec.decode(pkt)
            if h.profile == uiq.PROFILE_IQ_SCALED:
                scaled += 1
            silent += h.silent
            escape += h.escape
            if h.profile != last:
                changes += 1
                last = h.profile
            got.extend(s)
        self.assertEqual(len(got), len(want))
        self.assertEqual(got, want, 'iq-scaled does not decode to its restored samples')

        # The vector is only worth trusting if it still contains every case it
        # claims to: a shift of zero and several non-zero ones, a silent scaled
        # packet, an escaped scaled packet, and the crossings.
        self.assertIn(0, shifts)
        self.assertGreaterEqual(len(shifts), 4)
        self.assertGreater(scaled, 0)
        self.assertGreater(silent, 0)
        self.assertGreater(escape, 0)
        self.assertGreaterEqual(changes, 3)
        print('  iq-scaled        %d scaled, %d silent, %d escaped, %d changes, shifts %s'
              % (scaled, silent, escape, changes, sorted(shifts)))


class TestScaledProfile(unittest.TestCase):
    """Profile 2 in isolation: the quantise/restore pair and the encoder's
    scaled path, which the decode-only vector exercises but cannot pin down."""

    def test_quantise_rounds_and_clamps(self):
        # Rounding to nearest, not truncation: at ten bits half a step of the
        # coarse grid is 42 dB below the peak, not a rounding detail.
        self.assertEqual(uiq.quantise([0, 3, 4, 5, -3, -4, -5], 3),
                         [0, 0, 1, 1, 0, 0, -1])
        # Rounding can carry a full-scale peak one step past the ceiling, and
        # restoring that unclamped would wrap it to the opposite rail.
        self.assertEqual(uiq.quantise([32767], 3), [4095])
        self.assertEqual(uiq.quantise([-32768], 3), [-4096])
        # A shift of zero is the identity, and never modifies the caller's list.
        src = [1, -2, 3]
        self.assertEqual(uiq.quantise(src, 0), src)
        self.assertIsNot(uiq.quantise(src, 0), src)

    def test_restore_saturates(self):
        s = [4095, -4096, 1, 0, -1]
        uiq.restore(s, 3)
        self.assertEqual(s, [32760, -32768, 8, 0, -8])
        s = [8000]
        uiq.restore(s, 3)
        self.assertEqual(s, [32767])

    def test_scaled_round_trip(self):
        """A scaled stream must decode to the quantised-then-restored samples,
        and a silent scaled packet must carry no shift byte."""
        tone = [int(9000 * (1 if (i // 8) % 2 else -1)) for i in range(720)]
        enc = uiq.StreamEncoder()
        dec = uiq.StreamDecoder()
        hdr = uiq.HeaderDecoder()
        for shift in (0, 3, 5, 0):
            pkt = enc.encode_scaled(tone, 0, 384000, 2, -999, -999, shift)
            hh, off = hdr.decode(pkt)
            self.assertEqual(pkt[off], shift, 'the shift leads the body')
            h, out = dec.decode(pkt)
            self.assertEqual(h.profile, uiq.PROFILE_IQ_SCALED)
            expect = uiq.quantise(tone, shift)
            uiq.restore(expect, shift)
            self.assertEqual(out, expect)
        # Silence on the scaled profile: header only, no body and no shift.
        pkt = enc.encode_scaled([0] * 720, 0, 384000, 2, -999, -999, 5)
        hh, off = hdr.decode(pkt)
        self.assertEqual(len(pkt), off, 'a silent scaled packet must carry no shift byte')
        h, out = dec.decode(pkt)
        self.assertTrue(h.silent)
        self.assertEqual(out, [0] * 720)
        # And the taps must be where coding the zeros would have left them.
        pkt = enc.encode_scaled(tone, 0, 384000, 2, -999, -999, 3)
        hdr.decode(pkt)
        _, out = dec.decode(pkt)
        expect = uiq.quantise(tone, 3)
        uiq.restore(expect, 3)
        self.assertEqual(out, expect)

    def test_encoder_refuses_a_bad_request(self):
        enc = uiq.StreamEncoder()
        with self.assertRaises(uiq.Error):  # no scaled real profile
            enc.encode_scaled([0] * 240, 0, 12000, 1, -999, -999, 3)
        for shift in (-1, uiq.MAX_SHIFT + 1, 16, 255):
            with self.assertRaises(uiq.Error):
                enc.encode_scaled([1] * 720, 0, 384000, 2, -999, -999, shift)

    def test_decoder_refuses_a_bad_shift(self):
        """A shift past MAX_SHIFT, and a scaled packet with no body at all."""
        enc = uiq.StreamEncoder()
        pkt = enc.encode_scaled([1000] * 720, 0, 384000, 2, -999, -999, 3)
        _, off = uiq.HeaderDecoder().decode(pkt)
        bad = pkt[:off] + bytes((16,)) + pkt[off + 1:]
        with self.assertRaises(uiq.Error):
            uiq.StreamDecoder().decode(bad)
        with self.assertRaises(uiq.Error):
            uiq.StreamDecoder().decode(pkt[:off])


class TestRice(unittest.TestCase):
    def test_round_trip(self):
        cases = [
            [0] * 64,
            [1, -1, 2, -2, 3, -3],
            list(range(-500, 500)),
            # Long unary runs: one huge value among small ones, which is what a
            # strong carrier against a low noise floor produces.
            [0] * 200 + [30000] + [0] * 200,
            [-32768, 32767] * 100,
        ]
        for res in cases:
            with self.subTest(res[:4]):
                self.assertEqual(uiq.rice_decode(uiq.rice_encode(res), len(res)), res)

    def test_truncation_is_an_error(self):
        body = uiq.rice_encode(list(range(-100, 100)))
        with self.assertRaises(uiq.Error):
            uiq.rice_decode(body[:len(body) // 2], 200)


class TestCodec(unittest.TestCase):
    def test_escape_on_white_noise(self):
        """Full-scale noise cannot be predicted, so the body must not expand."""
        state = [2463534242]

        def rnd():
            x = state[0]
            x ^= (x << 13) & 0xFFFFFFFF
            x ^= x >> 17
            x ^= (x << 5) & 0xFFFFFFFF
            state[0] = x
            return x

        enc = uiq.Codec(uiq.PROFILE_AUDIO)
        dec = uiq.Codec(uiq.PROFILE_AUDIO)
        for _ in range(20):
            s = [(rnd() >> 16) - 32768 for _ in range(240)]
            body, escape = enc.encode(s)
            self.assertTrue(escape)
            self.assertLessEqual(len(body), len(s) * 2)
            self.assertEqual(dec.decode(body, len(s), escape), s)

    def test_leak_truncates_towards_zero(self):
        """SPEC.md section 6.2.3. This is the Python trap: ``>>`` FLOORS towards
        negative infinity, so ``w >> s`` is a different value on every negative
        tap -- and negative taps are the common case."""
        for shift in (14, 16):
            unit = 1 << shift
            for mag in (0, 1, unit - 1, unit, unit + 1, 3 * unit + 7, 1 << 23):
                self.assertEqual(uiq._leak(mag, shift), mag >> shift)
                self.assertEqual(uiq._leak(-mag, shift), -(mag >> shift))
        # Below 2**shift a tap leaks nothing at all, which is what stops the
        # leak fighting the gradient at small taps.
        self.assertEqual(uiq._leak(-((1 << 14) - 1), 14), 0)
        # And the floored form would be wrong here, which is the whole point.
        self.assertEqual(uiq._leak(-1, 14), 0)
        self.assertEqual(-1 >> 14, -1)

    def test_silence_advances_identically(self):
        """Skipping the body must leave the filters exactly where coding the
        zeros would have."""
        tone = [int(9000 * (1 if (i // 8) % 2 else -1)) for i in range(240)]
        zeros = [0] * 240
        coded = uiq.Codec(uiq.PROFILE_AUDIO)
        skipped = uiq.Codec(uiq.PROFILE_AUDIO)
        for _ in range(5):
            coded.encode(tone)
            skipped.encode(tone)
        for _ in range(10):
            coded.encode(zeros)
            skipped.advance_silence(len(zeros))
        self.assertEqual(coded.encode(tone), skipped.encode(tone))

    def test_unknown_profile_is_refused(self):
        """Ids 3-7 are unassigned. An unknown one must fail rather than fall
        back to profile 0: decoding with the wrong predictor does not raise an
        error, it returns plausible-sounding noise."""
        for pid in (3, 4, 7):
            with self.assertRaises(uiq.Error):
                uiq.Codec(pid)
        # 0, 1 and 2 are all defined, and 2 is the scaled one.
        self.assertFalse(uiq.Codec(uiq.PROFILE_IQ).scaled)
        self.assertFalse(uiq.Codec(uiq.PROFILE_AUDIO).scaled)
        self.assertTrue(uiq.Codec(uiq.PROFILE_IQ_SCALED).scaled)
        self.assertEqual(uiq.Codec(uiq.PROFILE_IQ_SCALED).samples_per_step, 2)


class TestQuality(unittest.TestCase):
    def test_sentinel_and_range(self):
        for v, want in [(-999, -999), (-1000, -999), (0, 0), (-42.5, -42.5),
                        (327.67, 327.67), (500, 327.67), (float('inf'), 327.67),
                        (float('-inf'), -999), (float('nan'), -999)]:
            self.assertAlmostEqual(
                uiq.quality_to_float(uiq.quality_from_float(v)), want, places=4)


if __name__ == '__main__':
    unittest.main(verbosity=2)
