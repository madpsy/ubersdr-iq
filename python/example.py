#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""A worked example: compress a raw sample file, decompress it, and check it.

    python3 python/example.py testdata/audio-cw-12k.pcm --rate 12000
    python3 python/example.py testdata/iq-384k.pcm --rate 384000 --channels 2 --frames 360

Sample files are raw little-endian int16, interleaved I/Q for two channels --
the same layout as the body of a WAV file.

This implementation runs at interpreter speed and is meant to be read, not
deployed; a wideband stream will not keep up in real time. See "Cost" at the
end of ubersdr_iq.py.
"""

import argparse
import os
import struct
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import ubersdr_iq as uiq  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('input', help='raw little-endian int16 samples')
    ap.add_argument('--rate', type=int, required=True, help='sample rate in Hz')
    ap.add_argument('--channels', type=int, default=1,
                    help='1 for mono audio, 2 for interleaved I/Q')
    ap.add_argument('--frames', type=int, default=240, help='frames per packet')
    ap.add_argument('--out', help='write the compressed stream here (.uiq)')
    args = ap.parse_args()

    with open(args.input, 'rb') as f:
        raw = f.read()
    samples = list(struct.unpack('<%dh' % (len(raw) // 2), raw))

    per = args.frames * args.channels
    if len(samples) < per:
        print('input is shorter than one packet')
        return 1

    # One encoder for the whole stream. It carries the predictor's adaptation
    # from packet to packet, which is where most of the compression comes from,
    # so it must not be recreated per packet.
    enc = uiq.StreamEncoder()
    packets = []
    t0 = time.perf_counter()
    for i in range(0, len(samples) - per + 1, per):
        # A real sender takes this from its receiver's clock. Here it is derived
        # from the sample index, which is what a steady stream looks like.
        ts = 1_700_000_000_000_000_000 + (i // args.channels) * 10**9 // args.rate
        packets.append(enc.encode(samples[i:i + per], ts, args.rate, args.channels))
    t1 = time.perf_counter()

    # One decoder for the whole stream, fed every packet in order. Skipping one
    # would leave its filters where the encoder's no longer are.
    dec = uiq.StreamDecoder()
    back = []
    for n, pkt in enumerate(packets):
        header, s = dec.decode(pkt)
        back.extend(s)
        if n == 0:
            print('first packet: %d bytes of header and body, profile %d (%s)'
                  % (len(pkt), header.profile,
                     uiq.PROFILE_NAMES.get(header.profile, '?')))
    t2 = time.perf_counter()

    coded = len(packets) * per
    if back != samples[:coded]:
        print('MISMATCH: the decoded samples differ from the input')
        return 1

    wire = sum(len(p) for p in packets)
    pps = args.rate / args.frames
    print('%s: %d samples, %d Hz, %d channel(s), %d frames/packet'
          % (args.input, coded, args.rate, args.channels, args.frames))
    print('  %d packets, %d -> %d bytes, %.3fx (%.1f%% saved), lossless'
          % (len(packets), coded * 2, wire, coded * 2 / wire,
             100 * (1 - wire / (coded * 2))))
    print('  encode %8.1f us/packet, decode %8.1f us/packet (%.0f packets/s in real time)'
          % ((t1 - t0) * 1e6 / len(packets), (t2 - t1) * 1e6 / len(packets), pps))

    if args.out:
        with open(args.out, 'wb') as f:
            uiq.write_stream(f, packets)
        print('  wrote %s' % args.out)
    return 0


if __name__ == '__main__':
    sys.exit(main())
