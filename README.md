# UberSDR-IQ

Lossless compression for software-defined radio sample streams — complex
baseband IQ and demodulated audio, as 16-bit integers.

Measured on live receiver traffic, whole packets against raw `int16` — nine
captures of 600 consecutive packets each:

| stream | raw | compressed | saved |
|---|---|---|---|
| CW, 12 kHz audio | 24.0 kB/s | 5.9 kB/s | 76% |
| SSB voice, 12 kHz audio | 24.0 kB/s | 11.0 kB/s | 54% |
| SSB data, 12 kHz audio | 24.0 kB/s | 13.2 kB/s | 45% |
| AM, 24 kHz audio | 48.0 kB/s | 28.4 kB/s | 41% |
| IQ, 384 kHz, medium wave | 1536.0 kB/s | 856.7 kB/s | 44% |
| IQ, 384 kHz, busy band | 1536.0 kB/s | 1040.3 kB/s | 32% |
| IQ, 12 kHz (`iq` mode) | 48.0 kB/s | 34.9 kB/s | 27% |
| squelched (digital silence) | 24.0 kB/s | 0.45 kB/s | 98% |

On the same data, the deployment's own zstd path returns 0.97–0.99× — output
*larger* than the input. Reference libzstd at level 3 manages 0.99–1.15×,
compressing AM and a quiet wideband segment slightly rather than expanding them;
neither is close to the table above.

**The ratio holds across the whole rate range.** The same band at 14.175 MHz,
captured back to back at every IQ rate the receiver offers, compresses to
1.35×, 1.38×, 1.36×, 1.35× and 1.35× for `iq48` through `iq384` — flat across
a 32:1 span in sample rate. The spread in the table above is the *band*, not the
rate: what a segment costs depends on what is in it.

**Reduced depth, when a client asks for one.** A client may request a *margin* in
decibels rather than a bit depth — how far below the band's own noise floor the
quantisation floor must stay — and the sender picks the depth that honours it,
per packet. At the reference default of 26 dB:

| stream | lossless | 26 dB margin | 15 dB margin | 10 dB margin |
|---|---|---|---|---|
| IQ, 384 kHz, quiet band | 1.54× | 2.91× (+30.2 dB) | 4.41× (+18.4 dB) | 5.46× (+13.9 dB) |
| IQ, 384 kHz, busy band | 1.48× | 2.45× (+28.9 dB) | 3.40× (+18.0 dB) | 4.14× (+13.3 dB) |
| IQ, 12 kHz (`iq` mode) | 1.38× | 2.32× (+28.6 dB) | 3.18× (+17.9 dB) | 3.79× (+12.6 dB) |

The margin delivered is always at least the margin asked for. On a band too
peaky to afford a coarser quantiser the sender returns a shift of zero and the
stream stays bit-exact, which is the promise working rather than failing.

**Cost.** A 384 kHz IQ stream costs about 1.0–1.4% of one core to encode or
decode on x86-64 with AVX2, 2.7% on a Raspberry Pi 5 with NEON, and about 3%
where neither is used; the reduced-depth path costs the same as the lossless
one to within 4%. Encode and decode cost about the same, so a client pays what
the server pays.

**Against FLAC.** A streaming codec cannot look ahead past the packet it is
sending, and every packet has to decode on its own because the one before it may
not have arrived. Held to that same constraint — 240-sample blocks — this format
beats `flac -8` on 7 of 11 measured streams:

| stream | UberSDR-IQ | `flac -8` @240 | |
|---|---|---|---|
| CW, 12 kHz | **4.10×** | 3.05× | **+34%** |
| SSB data (FT8), 12 kHz | **1.82×** | 1.48× | **+23%** |
| SSB voice, 12 kHz | **2.18×** | 1.84× | **+19%** |
| IQ, 12 kHz | **1.38×** | 1.28× | **+8%** |
| IQ, 384 kHz busy band | **1.48×** | 1.41× | **+5%** |
| AM broadcast, 24 kHz | 2.25× | **2.65×** | −15% |

The wins are on the traffic a receiver actually carries. The loss is on a
stationary carrier with shallow modulation, which is the best case for FLAC's
solved-per-block LPC and the worst for a backward-adaptive filter — see
[the full comparison](bitdepth-iq/flac-comparison.md) for all eleven streams,
timings, and the bit-exactness checks.

The two formats answer different questions. FLAC compresses a file that already
exists, with the whole recording in front of it; this compresses a stream that
must survive a lost packet, and transmits nothing about its predictor to do it.

## Why a general-purpose compressor does not work

zstd, deflate and their relatives are LZ77 matchers over bytes: they look for
repeated byte strings, and a band-limited RF signal has none. They correctly
conclude the data is incompressible, emit a stored block, and charge a frame
header for the privilege.

"No repeated strings" is not "no redundancy". Consecutive samples of a
band-limited signal are correlated — lag-1 correlation from 0.03 on an empty
wideband segment to 0.99 on narrowband AM. UberSDR-IQ predicts each sample from
the ones before it with an adaptive filter and Rice-codes the prediction error,
which turns that correlation into the table above.

The predictor is *backward* adaptive: its coefficients are derived from samples
that have already been coded, so the decoder recomputes them independently and
nothing about them is transmitted. All arithmetic is integer with specified
rounding, so encoder and decoder agree bit for bit across languages and
platforms — which is what makes a lossless claim mean something in a system
where the sender is Go, one client is C++ and another is a browser.

Read [SPEC.md](SPEC.md) for the format. It is written to be implementable from
alone, and it records the measurements behind each decision, including the
alternatives that lost.

## Implementations

| | encode | decode | notes |
|---|---|---|---|
| [`go/`](go/) | yes | yes | the reference; also the `uiq` command-line tool |
| [`python/`](python/) | yes | yes | pure Python, no dependencies, written to be read |
| [`cpp/`](cpp/) | yes | yes | single header, C++17, no exceptions |
| [`js/`](js/) | — | yes | ES module; exact with plain numbers, no BigInt |

All four reproduce the conformance vectors sample for sample; the first three
also reproduce the reference encoder byte for byte.

    ./conformance/run.sh

## Quick start

### Command line

```sh
go -C go build -o ../uiq ./cmd/uiq

./uiq encode -rate 12000 -channels 1 -frames 240 in.pcm out.uiq
./uiq decode out.uiq back.pcm
./uiq stat   out.uiq
./uiq bench  -rate 384000 -channels 2 -frames 360 in.pcm
```

Sample files are raw little-endian `int16`, interleaved I/Q for two channels —
the same layout as the body of a WAV file.

### Go

```go
enc := ubersdriq.NewStreamEncoder()
pkt, err := enc.Encode(samples, timestampNanos, 12000, 1, -30.5, -95.2)

dec := ubersdriq.NewStreamDecoder()
header, samples, err := dec.Decode(pkt)
```

### Python

```python
import ubersdr_iq as uiq

enc = uiq.StreamEncoder()
pkt = enc.encode(samples, timestamp_nanos, 12000, 1)

dec = uiq.StreamDecoder()
header, samples = dec.decode(pkt)
```

### C++

```cpp
#include "ubersdr_iq.hpp"

ubersdr_iq::StreamEncoder enc;
std::vector<uint8_t> pkt;
std::string err;
enc.Encode(samples.data(), samples.size(), ts, 12000, 1, -999, -999, &pkt, &err);

ubersdr_iq::StreamDecoder dec;
ubersdr_iq::Header h;
std::vector<int16_t> out;
dec.Decode(pkt.data(), pkt.size(), &h, &out, &err);
```

### JavaScript

```js
import { StreamDecoder } from './ubersdr-iq.mjs';

const dec = new StreamDecoder();
const r = dec.decode(frame);       // { samples, sampleRate, channels, signal, ... }
```

## Three things to know before using it

**A codec instance is a stream.** It carries the predictor's adaptation from
packet to packet — that is where the compression comes from — so create one per
stream, use it from one thread, and drop it when the stream ends. Do not pool
it and do not recreate it per packet.

**Every packet must reach the decoder, in order, from the first.** A packet that
is skipped — even one whose samples you intend to throw away — leaves the
decoder's filters where the encoder's no longer are, and everything after it
decodes to noise. The format needs an ordered, lossless transport.

**There is no random access.** Metadata resynchronises periodically, so a reader
that opens a stored stream part-way through learns the sample rate and channel
count; predictor state does not, so it cannot reconstruct the samples. SPEC.md
§8.3 gives the measurements behind that trade-off and what a container should do
if it needs seeking.

## Cost

Per packet, one x86-64 core, encode and decode within a few percent of each
other:

| stream | packets/s | per packet | % of a core |
|---|---|---|---|
| audio, 12 kHz | 50 | 16 µs | 0.09% |
| IQ, 12 kHz | 50 | 19 µs | 0.09% |
| IQ, 384 kHz | 1099 | 27 µs | 3.0% |

So a receiver serving twenty simultaneous wideband IQ sessions spends about 66%
of one core compressing them, and a client pays about what the server pays. The
pure-Python implementation is roughly a hundred times slower: fine for
demodulated audio and narrowband IQ in real time, not fast enough for wideband.

## Layout

```
SPEC.md            the format, in full, with the measurements behind it
go/ubersdriq/      reference encoder and decoder
go/cmd/uiq/        command-line tool: encode, decode, stat, bench
python/            encoder and decoder, plus example.py
cpp/               single-header encoder and decoder, plus example.cpp
js/                decoder as an ES module
testdata/          conformance vectors, and what each one is for
conformance/run.sh runs every implementation against them
LICENSE            AGPL-3.0-or-later, covering the code
LICENSE-DOCS       CC BY-SA 4.0, covering SPEC.md and the other prose
```

## Licence

Copyright (C) 2026 Nathan Higgins. Two licences, split by what the thing is.

**Code — AGPL-3.0-or-later.** Every implementation in this repository (`go/`,
`python/`, `cpp/`, `js/`, `conformance/`) and the test vectors in `testdata/`.
Full text in [LICENSE](LICENSE). The Affero clause matters here: this codec is
meant to run inside network services, and section 13 means a modified encoder
or decoder offered to users over a network has to offer its source too.

**Documentation — CC BY-SA 4.0.** The prose: [SPEC.md](SPEC.md), this README,
and `testdata/README.md`. Full text in [LICENSE-DOCS](LICENSE-DOCS). Reuse and
translate the specification freely, including commercially; keep the attribution
and license derived versions of the text alike.

Every source file carries an `SPDX-License-Identifier` line saying which of the
two applies to it.

Note on scope: these licences cover this code and this text. They do not, and
cannot, cover the format itself — an independent implementation written from
the specification is a new work, not a derivative of either. What the split buys
you is that *this* implementation and *this* specification text stay open as
they spread.
