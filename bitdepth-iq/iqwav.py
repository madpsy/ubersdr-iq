#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Capture handling: WAV in, raw IQ out, and a USB demodulator for the judge.

iq-recorder writes 16-bit stereo WAV with I in the left channel and Q in the
right, which is the codec's input layout once the 44-byte header is off.

    iqwav.py topcm  in.wav out.pcm [--trim SAMPLES_PER_PACKET]
    iqwav.py usb    in.pcm out.wav --rate HZ [--offset HZ] [--width HZ]
    iqwav.py info   in.wav
"""

import sys
import wave
import numpy as np


def read_wav(path):
    with wave.open(path, "rb") as w:
        if w.getnchannels() != 2 or w.getsampwidth() != 2:
            raise SystemExit(f"{path}: expected 16-bit stereo, got "
                             f"{w.getnchannels()}ch/{w.getsampwidth()*8}bit")
        rate = w.getframerate()
        x = np.frombuffer(w.readframes(w.getnframes()), dtype="<i2")
    return x, rate


def usb_demod(x, rate, out_rate=12000, offset=0.0, width=3000.0):
    """Complex baseband to real USB audio, the way a client's demodulator would.

    The whole point of running the judge through this rather than scoring the IQ
    directly is that a listener or a decoder never sees the IQ: it sees what
    came out of a demodulator, and quantisation noise spread across a 192 kHz
    span lands very differently in a 3 kHz slice of it.

    Upper sideband about `offset` is kept by zeroing every other bin, which is
    an ideal brick wall -- deliberately, so the measurement is of the codec and
    not of a filter design. The result is analytic, so its real part is the
    audio, and it is already band-limited well inside the output Nyquist, which
    is what lets the decimation be a plain stride.
    """
    c = x[0::2].astype(np.float64) + 1j * x[1::2].astype(np.float64)
    n = len(c)
    dec = int(round(rate / out_rate))
    if dec * out_rate != rate:
        raise SystemExit(f"rate {rate} is not an integer multiple of {out_rate}")

    C = np.fft.fft(c)
    f = np.fft.fftfreq(n, 1.0 / rate)
    C[~((f >= offset) & (f < offset + width))] = 0.0
    a = 2.0 * np.real(np.fft.ifft(C))
    a = a[::dec]

    peak = np.max(np.abs(a)) if len(a) else 0.0
    if peak > 0:
        a = a * (28000.0 / peak)      # headroom, jt9 only cares about shape
    return a.astype("<i2")


def write_wav(path, samples, rate):
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(samples.tobytes())


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    cmd = sys.argv[1]

    def opt(name, cast, default=None):
        return cast(sys.argv[sys.argv.index(name) + 1]) if name in sys.argv else default

    if cmd == "info":
        x, rate = read_wav(sys.argv[2])
        c = x[0::2].astype(np.float64) + 1j * x[1::2].astype(np.float64)
        print(f"{sys.argv[2]}: {rate} Hz, {len(c)} frames, {len(c)/rate:.2f} s, "
              f"peak {int(np.max(np.abs(x)))}, rms {np.sqrt(np.mean(np.abs(c)**2)):.1f}")
        return 0

    if cmd == "topcm":
        x, rate = read_wav(sys.argv[2])
        spp = opt("--trim", int)
        if spp:
            n = (len(x) // spp) * spp        # whole packets, so the control
            x = x[:n]                        # arm can round trip exactly
        x.tofile(sys.argv[3])
        print(f"{sys.argv[3]}: {rate} Hz, {len(x)//2} frames, {len(x)} samples")
        return 0

    if cmd == "usb":
        x = np.fromfile(sys.argv[2], dtype="<i2")
        rate = opt("--rate", int)
        a = usb_demod(x, rate, offset=opt("--offset", float, 0.0),
                      width=opt("--width", float, 3000.0))
        write_wav(sys.argv[3], a, 12000)
        print(f"{sys.argv[3]}: 12000 Hz, {len(a)} samples, {len(a)/12000:.2f} s")
        return 0

    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main())
