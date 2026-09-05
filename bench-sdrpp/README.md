# UberSDR-IQ compared with SDR++ server streaming

A measured comparison against the streaming compression in
[SDR++](https://github.com/AlexandreRouma/SDRPlusPlus) `sdrpp_server`, on real
IQ from a live receiver.

The short version, in two parts. Carrying the same samples losslessly,
UberSDR-IQ needs 132.6 kB/s where the lossless mode SDR++ ships needs
386.4 kB/s — a saving of 0.91 GB per session-hour — and it buys that with 14
CPU-seconds per hour of streaming. And where a client will accept an
approximation, UberSDR-IQ's reduced-depth profile at its default 26 dB margin
sends 75.6 kB/s against SDR++'s int8 at 98.4: **1.30× fewer bytes than the
smallest thing SDR++ can send, while holding its error 30.3 dB under the band's
own noise floor and clipping no sample at all, where int8 holds 24.9 dB and
clips 4432.** Saturated, one eight-core laptop part still serves 1380 concurrent
lossless 48 kHz sessions. Everything below is the working.

## The capture

`iq48-14M080.pcm` is 30.00 s of 48 kHz complex baseband centred on 14.080 MHz,
from a live UberSDR receiver on 2026-09-03 — 1,440,360 frames of raw
little-endian int16. `capture.py` recorded the stream and `uiq decode` wrote the
samples out:

    UBERSDR_HOST=receiver.example python3 capture.py iq48 14080000 30 live.uiq
    go -C ../go run ./cmd/uiq decode live.uiq iq48-14M080.pcm

Because the capture is lossless it stands in for the receiver's own samples, so
the same recording serves as both a compression measurement and the reference
against which the lossy modes are scored. The benchmark uses the 4501 whole
320-frame packets it contains, 1,440,320 frames, and drops the 40-frame tail.

### The `.uiq` that was here before

The `.uiq` committed alongside this file until 2026-09-05 was the live capture
as it came off the wire on 2026-09-03, which was **before the LMS tap leakage
change of 2026-09-04**. It no longer decodes. Run against the current decoder it
fails silently rather than loudly — it returns 2,880,720 samples with no error,
of which **2,863,657 (99.41%) are wrong**. The two streams agree for the first
8379 frames, packet 26, and diverge from there; the result reconstructs at an
SNR of **−15.1 dB**, which is to say the error is 32 times the signal. This is
exactly the failure §6.1 of SPEC.md describes for a mismatched predictor: not an
error, plausible-sounding noise.

The `.pcm` was unaffected — it is raw int16 and remains valid ground truth — so
the `.uiq` has been regenerated from it with the current encoder and committed
in its place. It round-trips bit-exactly. Every figure below is from the current
codec.

That leaves the old README's claim that re-encoding reproduces the live wire
size to within 0.03% needing to be restated rather than repaired, because the
two sides of it are now different codecs. Measured, all three on the same
1,440,320 frames:

| encoder | coded bytes | kB/s |
|---|---|---|
| pre-leak production encoder, live wire, 2026-09-03 | 3,959,238 | 132.0 |
| pre-leak reference encoder, re-encoding the `.pcm` | 3,954,869 | 131.8 |
| **current reference encoder** (`go`, and `cpp` in `bench`) | **3,978,480** | **132.6** |

So the reference encoder reproduced the live wire to within 0.11%, not 0.03%,
and that was a pre-leak measurement on both sides. Against the current encoder
the leakage change costs 0.60% on this capture — a real regression in bytes,
bought for the numerical property it was made for. **No live capture has been
taken with the current production encoder, so no live-wire figure is quoted for
it; the row above is not measured and is not estimated.** The receiver's address
is not in this repository and was not sought.

One check the regeneration does allow: the Go encoder that wrote the committed
`.uiq` and the C++ encoder `bench` links both produce **3,978,480 bytes** on this
capture, identical to the byte. The two implementations agree exactly.

## What is measured

SDR++ reduces bandwidth in two independent stages, in `core/src/server.cpp` and
`core/src/dsp/compression/`:

1. **Requantisation.** The stream is float32 complex. Per block the compressor
   takes a scaler from the block, writes it as a float32, and converts the block
   to int8 or int16 against it — a per-block adaptive gain. The client
   multiplies back by the same scaler.
2. **zstd level 1** over the quantised block, selected by
   `COMMAND_SET_COMPRESSION`.

`bench.cpp` transcribes `SampleStreamCompressor::process` and
`SampleStreamDecompressor::process` verbatim, calls the same volk kernels the
real build links, and uses real libzstd at level 1 as `server.cpp` does. The
UberSDR-IQ side is `../cpp/ubersdr_iq.hpp`. Both run in one binary on one core,
so bytes and timings are directly comparable.

Blocks are 320 frames on the live capture, matching its own packet cadence of
150/s, and 360 on the 384 kHz vectors. `bench.cpp` takes the block size as an
argument; from 320 to 8192 frames the SDR++ ratios move by about 1% and
UberSDR-IQ's by 1.2%, in its favour.

Two scaler variants appear for each integer mode. SDR++ takes its scaler from
`volk_32f_index_max_32u`, the largest signed value in the block; the
`peak fixed` rows take the largest magnitude instead and are otherwise
identical. Both are shown because the choice moves the results.

### Byte counting, stated before the numbers

**Ours.** The sum of `len(packet)` over every packet, per-packet headers
included. The `.uiq` container's 4-byte length prefix is *excluded*, because a
message transport supplies the frame boundary itself and a deployment does not
send it. It would add 18,004 bytes to the 48 kHz capture, 0.45%, and would not
change any verdict below.

**SDR++.** Every byte the server puts on the socket: the 8-byte `PacketHeader`
`server.cpp` wraps each baseband packet in, plus the 8-byte compressor header
(`compressionType`, `sampleType` and the float32 scaler), plus the payload —
and, on the zstd rows, the zstd frame in place of the payload. Nothing is
excluded on either side.

### The reduced-depth arms

SDR++'s int8 and int16 modes are lossy, and until now this comparison could only
concede int8 the byte count and note that it is not the samples. Since SPEC.md
§6.6 there is a lossy mode on this side too — profile 2, `iq-complex-o16-scaled`,
which is profile 0's predictor over samples requantised by a power of two, with
the shift travelling in the packet.

Choosing that shift is a sender policy and is not part of the format (§6.6.4).
The policy measured here is not invented for the benchmark: it is
`../bitdepth-iq/margin_rule.py`, which is what the reference sender does. A
client asks for a **margin** in decibels — how far below the band's own noise
floor the quantisation floor must be held — and the depth honouring it is worked
out per packet from the packet's crest factor and the band's spectral peakiness.
Two arms are run: **26 dB**, the reference default, and **15 dB**, the
aggressive end of the range the reference sender accepts.

`shifts.py` runs `margin_rule.quantise_margin` and writes one shift byte per
packet; `bench` hands each to `StreamEncoder::EncodeScaled`, so the
quantisation itself is SPEC.md §6.6.2's and not a numpy experiment's. The two
differ only in how they clamp, and only for a sample that rounding carries past
the ceiling: on the 48 kHz capture that is 5 samples in 2,880,640 at 26 dB and
26 at 15 dB, 0.0009% at worst, and none at all on the 384 kHz vectors.

## Running it

    ./run.sh

Needs `libvolk-dev`, `libzstd-dev`, a C++17 compiler and numpy.

## Results

### Bandwidth and fidelity

30 s of 48 kHz IQ at 14.080 MHz. `vs int16` is against the 192.0 kB/s the raw
int16 samples occupy; `SNR` and `max err` are against those same samples. Only
the rows marked lossless return the receiver's samples; the rest return an
approximation of them.

| mode | kB/s | vs int16 | lossless | SNR | max err | clipped | winner |
|---|---|---|---|---|---|---|---|
| SDR++ float32 | 386.4 | 0.497× | yes | exact | 0 | 0 | **UberSDR-IQ** — 2.9× fewer bytes, both exact |
| SDR++ float32 + zstd-1 | 269.5 | 0.712× | yes | exact | 0 | 0 | **UberSDR-IQ** — 2.0× fewer bytes, both exact |
| SDR++ int16 (server default) | 194.4 | 0.988× | no | 43.9 dB | 2910 | 4432 | **UberSDR-IQ** — 1.47× fewer bytes *and* exact |
| SDR++ int16 + zstd-1 | 195.8 | 0.980× | no | 43.9 dB | 2910 | 4432 | **UberSDR-IQ** — 1.48× fewer bytes *and* exact |
| SDR++ int8 | 98.4 | 1.951× | no | 41.5 dB | 2910 | 4432 | **UberSDR-IQ at 26 dB** — 1.30× fewer bytes, 5.7 dB better SNR, nothing clipped |
| SDR++ int8 + zstd-1 | 99.9 | 1.922× | no | 41.5 dB | 2910 | 4432 | **UberSDR-IQ at 26 dB** — 1.32× fewer bytes, on the same terms |
| SDR++ int16, peak fixed | 194.4 | 0.988× | yes | 93.2 dB | 0 | 0 | **UberSDR-IQ** — 1.47× fewer bytes, both exact |
| SDR++ int8, peak fixed | 98.4 | 1.951× | no | 45.0 dB | 110 | 0 | **UberSDR-IQ at 26 dB** — 1.30× fewer bytes, 2.2 dB better SNR |
| **UberSDR-IQ (lossless)** | **132.6** | **1.448×** | **yes** | exact | 0 | 0 | — |
| **UberSDR-IQ, 26 dB margin** | **75.6** | **2.540×** | no | 47.2 dB | 64 | **0** | — |
| **UberSDR-IQ, 15 dB margin** | **53.2** | **3.607×** | no | 35.9 dB | 256 | **0** | — |

Two results, and the second is the new one.

Among the modes that reconstruct the samples, UberSDR-IQ is the smallest by a
wide margin: 132.6 kB/s against 194.4 for a magnitude-scaled int16 and 386.4 for
the float32 mode `sdrpp_server` actually offers — 1.47× and 2.9× smaller, or
222 MB and 914 MB less per session-hour.

Among the modes that do not, UberSDR-IQ is now also the smallest, and it wins
without a caveat attached. At the reference sender's default margin it sends
1.30× fewer bytes than int8 *and* returns a better reconstruction on every axis
measured: 5.7 dB more SNR, a maximum error of 64 against 2910, and no clipped
sample against 4432. At 15 dB it sends 1.85× fewer bytes than int8 and gives up
5.6 dB of SNR to do it — which is the point rather than a defect: the margin it
was asked for is the margin it holds, and 15 dB is the aggressive end of the
range on purpose.

The max-error column is the clearest statement of the difference in kind. A
margin-based quantiser rounds and never clips, so its worst case is exactly half
a step of the coarsest packet it sent: 64 at 26 dB, where the coarsest shift was
7, and 256 at 15 dB, where it was 9. Both are arithmetic, not luck. SDR++'s
2910 is a rail: the scaler is the largest *signed* value in the block, so any
sample more negative than that is clipped outright, 4432 times on this capture.
The `peak fixed` variant removes exactly that fault, and cheaply: 0.9 µs per
block against 0.7 here, and that is a scalar loop where the real fix would be a
volk magnitude kernel. Worth saying plainly, because it is a bug in the shipping
code rather than a property of requantisation — nothing about sending 8-bit
samples requires clipping 4432 of them.

Against the two 384 kHz vectors in `../testdata`, on the same 1536.0 kB/s of raw
int16:

| mode | `iq-384k-mw` kB/s | ratio | `iq-384k` kB/s | ratio | winner |
|---|---|---|---|---|---|
| SDR++ float32 | 3089.1 | 0.497× | 3089.1 | 0.497× | **UberSDR-IQ** — 3.2× fewer bytes, both exact |
| SDR++ float32 + zstd-1 | 2207.4 | 0.696× | 1874.1 | 0.820× | **UberSDR-IQ** — 1.9–2.3× fewer bytes, both exact |
| SDR++ int16 (server default) | 1553.1 | 0.989× | 1553.1 | 0.989× | **UberSDR-IQ** — 1.6× fewer bytes *and* exact |
| SDR++ int16 + zstd-1 | 1563.7 | 0.982× | 1559.7 | 0.985× | **UberSDR-IQ** — 1.6× fewer bytes *and* exact |
| SDR++ int8 | 785.1 | 1.957× | 785.1 | 1.957× | split — **UberSDR-IQ at 26 dB** on the busy band, SDR++ by 0.95% on medium wave |
| SDR++ int8 + zstd-1 | 795.7 | 1.930× | 794.0 | 1.934× | **UberSDR-IQ at 26 dB** on the busy band; at 15 dB on both |
| **UberSDR-IQ (lossless)** | **975.2** | **1.575×** | **970.1** | **1.583×** | — |
| **UberSDR-IQ, 26 dB margin** | **880.2** | **1.745×** | **600.2** | **2.559×** | — |
| **UberSDR-IQ, 15 dB margin** | **792.6** | **1.938×** | **443.3** | **3.465×** | — |

**Medium wave is the loss, and it is a real one.** Asked for 26 dB there, the
policy takes a mean shift of 1.01 — barely any reduction at all — and saves
9.7%; at 15 dB it takes 1.96 and saves 18.7%, ending at 792.6 kB/s against
int8's 785.1. SDR++ sends 0.95% fewer bytes. It does so at a 3.0 dB margin and a
1.78 dB rise in the band's own noise floor, against 35.6 dB and 0.00 dB, so the
two are not really trading the same thing — but on the byte count alone, on that
capture, int8 wins and the table says so. Medium wave carriers sit 67.6 dB above
the noise and genuinely need the depth; SPEC.md §6.6.4 records the same result
from the other direction, 15.6% saved at a 20 dB request where a quiet band
saves 60.6%.

On the busy amateur band the reduced-depth arms save 38.1% and 54.3% against our
own lossless mode, and 600.2 kB/s at 26 dB is 1.31× smaller than int8's 785.1.
On the 48 kHz capture the same two arms save 43.0% and 59.9%.

The SDR++ integer modes return a fixed ratio set by the word size — 0.989× and
1.957× on every capture — because requantisation cannot know anything about the
signal. UberSDR-IQ's ratio follows the band: 1.448× on 20 m, 1.583× on a busy
amateur band, 1.575× on medium wave. At 384 kHz that is 2.1 GB per session-hour
it does not send losslessly. SPEC.md §10.1 records 60× on a squelched channel,
where a silent packet costs no body at all.

The zstd stage measures as SPEC.md §2 describes. On int16 it added 0.7% to the
packet and on int8 1.5%, at 320-frame blocks; at 8192 frames it becomes
marginally useful on int8 (2.115× against 1.998×) and stays negative on int16
(1.005× against 1.000×). Its gain on float32 is real but comes from provenance
rather than from the signal: the source is int16-native, so every float32
mantissa carries sixteen trailing zero bits.

### What the lossy modes do to the band

Error signal measured against the original samples, as a power spectral density
over 4096-bin Hann periodograms. `margin` is how far the error floor sits below
the band's own noise floor — the quantity a reduced-depth stream is *asked* for,
put on the same axis for the SDR++ modes, which promise no such thing.
`floor rise` is how much the band's own noise floor moves once the error is
added; `headroom` is the strongest signal in the band above the error floor.

| capture | peak-to-floor | mode | margin | floor rise | headroom |
|---|---|---|---|---|---|
| 14.080 MHz, 48 kHz | 44.6 dB | SDR++ int16 (default) | 27.5 dB | 0.01 dB | 72.1 dB |
| | | SDR++ int16, peak fixed | — | 0.00 dB | bit exact |
| | | SDR++ int8 | 24.9 dB | 0.01 dB | 69.5 dB |
| | | SDR++ int8, peak fixed | 28.2 dB | 0.01 dB | 72.8 dB |
| | | **UberSDR-IQ, 26 dB asked** | **30.3 dB** | **0.00 dB** | **74.9 dB** |
| | | **UberSDR-IQ, 15 dB asked** | **19.1 dB** | 0.05 dB | 63.7 dB |
| `iq-384k` busy band | 37.8 dB | SDR++ int16 (default) | 22.8 dB | 0.02 dB | 60.5 dB |
| | | SDR++ int16, peak fixed | — | 0.00 dB | bit exact |
| | | SDR++ int8 | 22.2 dB | 0.03 dB | 60.0 dB |
| | | SDR++ int8, peak fixed | 30.8 dB | 0.00 dB | 68.6 dB |
| | | **UberSDR-IQ, 26 dB asked** | **28.2 dB** | 0.01 dB | 66.0 dB |
| | | **UberSDR-IQ, 15 dB asked** | **16.5 dB** | 0.10 dB | 54.3 dB |
| `iq-384k-mw` medium wave | 67.6 dB | SDR++ int16 (default) | 4.6 dB | 1.29 dB | 72.2 dB |
| | | SDR++ int16, peak fixed | — | 0.00 dB | bit exact |
| | | SDR++ int8 | 3.0 dB | 1.78 dB | 70.6 dB |
| | | SDR++ int8, peak fixed | 7.5 dB | 0.71 dB | 75.1 dB |
| | | **UberSDR-IQ, 26 dB asked** | **42.4 dB** | **0.00 dB** | **110.0 dB** |
| | | **UberSDR-IQ, 15 dB asked** | **35.6 dB** | **0.00 dB** | **103.2 dB** |

Read the `margin` column against what each mode was asked for and the difference
between the two approaches is the whole story. **Every UberSDR-IQ row delivers at
least the margin requested, on every capture, and none of the six undershoots.**
That is the direction to be wrong in: the closed-form estimate in
`margin_rule.py` spends bits where it is uncertain and never withholds them.

How far each overshoots depends on how much of the capture the estimator got to
see, and the three captures are not comparable on that point. Peakiness refreshes
on a 100 ms cadence over a 1 s window. The 48 kHz capture is 30 s, so it takes
**300** readings and only its first 3 packets fall back to the "assume the worst"
60 dB default; its margins land at +4.3 and +4.1 dB over the request, which is
the figure to quote for a running stream. The 384 kHz vectors are 0.075 s long,
so **exactly one** reading is ever taken — from the first 3 ms — and held for all
80 packets, with the first 2 on the fallback. Those two run open-loop: the busy
band still lands close, +2.2 and +1.5 dB, and medium wave overshoots by 16.4 and
20.6 dB. **The medium wave figures are therefore not evidence that the policy
tracks that band well — they are what a single 3 ms reading of it produced, and
the bytes in the previous table are what that reading cost.** A 30 s medium wave
capture would settle it; none was taken, so it is not claimed.

The SDR++ rows have no such column to check against, because there is nothing to
ask for. Per-block adaptive gain buys the 8-bit modes roughly 70–75 dB of usable
range, which is more than either HF capture occupies, so on both the error lands
below the band's own noise floor and the floor moves by hundredths of a dB. That
headroom is not a constant, though — the scaler tracks the loudest thing in each
block, so the error floor rises with the band and costs most exactly when the
band is busiest. Medium wave, with carriers 67.6 dB above the floor, takes 0.71
to 1.78 dB of floor and is left with 3.0 dB of margin on the default int8 path.
Under a margin request it takes 0.00 dB of floor, and pays in bytes instead —
which is the trade being made explicit rather than discovered afterwards.

For the lossless profile none of this is a parameter to check at all. The error
is zero on every capture, at every rate, whatever appears in the passband, and
no future band condition can make it otherwise.

### CPU cost

Thread CPU time (`CLOCK_THREAD_CPUTIME_ID`), not wall clock, on the reference
machine below. Encoder side; a decoder pays about the same, which is what the
`dec µs` column shows. Every figure is for **one** real-time stream, and every
figure is the **mean of three full runs**, with the min–max range given for the
columns that move:

- **CPU ms/s** — CPU milliseconds spent per second of signal
- **% core** — share of one core, `CPU ms/s ÷ 10`
- **CPU s/h** — CPU seconds burned per hour of streaming
- **streams** — concurrent real-time streams one core sustains

48 kHz IQ, 320-frame blocks, 150 blocks/s:

| mode | enc µs/blk | dec µs/blk | Msample/s | CPU ms/s | % core | CPU s/h (range) | streams/core | winner |
|---|---|---|---|---|---|---|---|---|
| SDR++ float32 | 0.7 | 0.5 | 897 | 0.106 | 0.011% | 0.38 (0.37–0.41) | 9345 | SDR++ — 13.8 CPU-s/h cheaper |
| SDR++ float32 + zstd-1 | 11.6 | 5.0 | 55 | 1.747 | 0.175% | 6.29 (6.23–6.33) | 573 | SDR++ — 7.9 CPU-s/h cheaper |
| SDR++ int16 (server default) | 0.7 | 0.5 | 871 | 0.111 | 0.011% | 0.40 (0.40–0.40) | 9076 | SDR++ — 13.8 CPU-s/h cheaper |
| SDR++ int16 + zstd-1 | 4.9 | 0.6 | 132 | 0.730 | 0.073% | 2.63 (2.60–2.68) | 1370 | SDR++ — 11.5 CPU-s/h cheaper |
| SDR++ int8 | 0.7 | 0.5 | 864 | 0.112 | 0.011% | 0.40 (0.40–0.41) | 8995 | SDR++ — 13.8 CPU-s/h cheaper |
| SDR++ int8 + zstd-1 | 5.5 | 0.6 | 118 | 0.815 | 0.081% | 2.93 (2.79–3.22) | 1232 | SDR++ — 11.2 CPU-s/h cheaper |
| SDR++ int16, peak fixed | 0.9 | 0.5 | 682 | 0.142 | 0.014% | 0.51 (0.50–0.52) | 7108 | SDR++ — 13.6 CPU-s/h cheaper |
| SDR++ int8, peak fixed | 0.9 | 0.5 | 682 | 0.142 | 0.014% | 0.51 (0.50–0.52) | 7108 | SDR++ — 13.6 CPU-s/h cheaper |
| **UberSDR-IQ (lossless)** | **26.2** | **27.9** | **24** | **3.93** | **0.393%** | **14.15 (13.65–15.08)** | **255** | — |
| **UberSDR-IQ, 26 dB margin** | **26.8** | **28.1** | **24** | **4.02** | **0.402%** | **14.48 (14.22–14.94)** | **249** | — |
| **UberSDR-IQ, 15 dB margin** | **26.8** | **28.1** | **24** | **4.01** | **0.401%** | **14.45 (14.08–15.07)** | **249** | — |

Two things to take from it.

CPU is the column SDR++ wins, and the margin is worth putting in proportion: the
whole of UberSDR-IQ's advantage costs 13.8 CPU-seconds per hour of streaming,
0.39% of one core, on a 15 W laptop part. A single core still carries 255
concurrent 48 kHz sessions. What those seconds buy is 222 MB per session-hour
against SDR++'s int16, or 914 MB against the float32 mode it ships — and the
samples arriving unaltered.

And the reduced-depth arms cost the *same* CPU as the lossless one, not less:
14.48 and 14.45 CPU-s/h against 14.15, a 2% difference inside a run-to-run
spread of 10%. Profile 2 is profile 0's predictor with a quantiser in front of
it, so the predictor still runs on every sample and the requantisation adds a
pass rather than removing one. **The reduced-depth mode saves bytes, not
cycles.** Anyone reaching for it to cut CPU is reaching for the wrong thing.

Worth reading alongside them: `int16 + zstd-1` spends 2.63 CPU-s/h, more than
six times the plain convert, and returns a packet 0.7% *larger*. Where a
receiver already has CPU budget for a compressor, that is what it is currently
spending it on.

384 kHz IQ, 360-frame blocks, 1067 blocks/s (`iq-384k-mw` / `iq-384k`), means of
three runs:

| mode | CPU ms/s | % core | CPU s/h | streams/core |
|---|---|---|---|---|
| SDR++ float32 | 0.62 / 0.70 | 0.062% / 0.070% | 2.25 / 2.51 | 1609 / 1435 |
| SDR++ float32 + zstd-1 | 14.06 / 18.56 | 1.406% / 1.856% | 50.6 / 66.8 | 72 / 54 |
| SDR++ int16 (server default) | 0.76 / 0.74 | 0.076% / 0.074% | 2.73 / 2.68 | 1321 / 1343 |
| SDR++ int16 + zstd-1 | 6.69 / 6.84 | 0.669% / 0.684% | 24.1 / 24.6 | 149 / 146 |
| SDR++ int8 | 0.83 / 0.76 | 0.083% / 0.076% | 2.98 / 2.72 | 1216 / 1323 |
| SDR++ int8 + zstd-1 | 7.39 / 6.74 | 0.739% / 0.674% | 26.6 / 24.3 | 136 / 148 |
| SDR++ int16, peak fixed | 1.00 / 1.01 | 0.100% / 0.101% | 3.59 / 3.63 | 1004 / 993 |
| SDR++ int8, peak fixed | 1.06 / 1.03 | 0.106% / 0.103% | 3.83 / 3.71 | 944 / 974 |
| **UberSDR-IQ (lossless)** | **33.72 / 30.96** | **3.372% / 3.096%** | **121.4 / 111.5** | **30 / 32** |
| **UberSDR-IQ, 26 dB margin** | **34.44 / 32.39** | **3.444% / 3.239%** | **124.0 / 116.6** | **29 / 31** |
| **UberSDR-IQ, 15 dB margin** | **32.69 / 32.54** | **3.269% / 3.254%** | **117.7 / 117.1** | **31 / 31** |

SDR++'s requantisation is a vectorised convert and a memcpy, so it runs at
725–1236 Msample/s. UberSDR-IQ's predictor is sequential and adapts per sample,
which puts it at 23 Msample/s. SPEC.md §10.2 reports 3.0% of a core for a
384 kHz stream; this machine measures 3.1–3.4%, and it is a 15 W laptop part
clocking around 2.2 GHz under the run.

### Saturation: what the whole machine actually serves

Scaling a single-thread figure by the core count assumes a machine that does not
exist. These tables are measured instead: N encoders run in parallel, each on
its own copy of the samples, flat out for two seconds, and aggregate throughput
is divided by one stream's real-time rate. The result is the number of
concurrent sessions that fit, paying for SMT contention, shared L3, memory
bandwidth and whatever the clock does under all-core load. Every cell is the
mean of three runs.

Concurrent real-time **48 kHz** streams sustained:

| mode | 1 thread | 2 | 4 | 8 | 16 | peak (range) |
|---|---|---|---|---|---|---|
| SDR++ float32 | 35665 | 69104 | 100802 | 99899 | 97001 | 101584 (98388–105716) |
| SDR++ float32 + zstd-1 | 536 | 1016 | 1883 | 2992 | 3766 | 3766 (3175–4441) |
| SDR++ int16 (server default) | 23090 | 46072 | 79111 | 103948 | 102653 | 104150 (101923–107830) |
| SDR++ int16 + zstd-1 | 1418 | 2743 | 4688 | 7483 | 8895 | 8895 (8506–9317) |
| SDR++ int8 | 22589 | 44842 | 75718 | 102545 | 105012 | 105012 (104036–106290) |
| SDR++ int8 + zstd-1 | 1343 | 2467 | 4096 | 6753 | 8138 | 8138 (7752–8346) |
| SDR++ int16, peak fixed | 13204 | 24534 | 42028 | 62117 | 69022 | 69022 (61621–75274) |
| SDR++ int8, peak fixed | 12329 | 24238 | 40427 | 63146 | 72595 | 72595 (69462–74392) |
| **UberSDR-IQ (lossless)** | **239** | **429** | **769** | **1224** | **1380** | **1380 (1354–1428)** |
| **UberSDR-IQ, 26 dB margin** | **230** | **408** | **696** | **1130** | **1307** | **1307 (1228–1380)** |
| **UberSDR-IQ, 15 dB margin** | **211** | **401** | **714** | **1187** | **1340** | **1340 (1272–1409)** |

Concurrent real-time **384 kHz** streams sustained (`iq-384k-mw` / `iq-384k`):

| mode | 1 thread | 2 | 4 | 8 | 16 | peak |
|---|---|---|---|---|---|---|
| SDR++ float32 | 22917 / 21557 | 39541 / 39927 | 66811 / 69728 | 97316 / 103946 | 94850 / 95313 | 97657 / 103946 |
| SDR++ float32 + zstd-1 | 76 / 54 | 143 / 101 | 247 / 170 | 400 / 271 | 485 / 319 | 485 / 319 |
| SDR++ int16 (server default) | 4332 / 3891 | 7945 / 7406 | 13369 / 12385 | 21716 / 20405 | 25248 / 25674 | 25248 / 25674 |
| SDR++ int16 + zstd-1 | 155 / 148 | 296 / 274 | 500 / 459 | 794 / 732 | 942 / 885 | 942 / 885 |
| SDR++ int8 | 3931 / 3672 | 7181 / 6655 | 12063 / 11286 | 19158 / 18693 | 25261 / 23839 | 25261 / 23839 |
| SDR++ int8 + zstd-1 | 153 / 151 | 283 / 279 | 479 / 480 | 774 / 758 | 921 / 952 | 921 / 952 |
| SDR++ int16, peak fixed | 2064 / 1977 | 3669 / 3530 | 6451 / 5985 | 10380 / 10017 | 11300 / 11150 | 11300 / 11150 |
| SDR++ int8, peak fixed | 1953 / 1900 | 3559 / 3481 | 6013 / 5890 | 9904 / 9597 | 10987 / 10606 | 10987 / 10606 |
| **UberSDR-IQ (lossless)** | **29 / 30** | **55 / 58** | **98 / 98** | **153 / 156** | **167 / 180** | **167 / 180** |
| **UberSDR-IQ, 26 dB margin** | **27 / 29** | **55 / 54** | **91 / 92** | **140 / 151** | **165 / 174** | **165 / 174** |
| **UberSDR-IQ, 15 dB margin** | **27 / 30** | **52 / 56** | **90 / 94** | **133 / 146** | **162 / 177** | **162 / 177** |

**One 15 W laptop part serves 1380 concurrent lossless 48 kHz sessions, or
167–180 concurrent 384 kHz sessions.** That is measured on all sixteen threads,
not extrapolated, and it is comfortably beyond what any deployed WebSDR carries:
fifty simultaneous 48 kHz sessions would use 3.6% of this machine, and a hundred
simultaneous 384 kHz sessions — an implausible load for a single receiver — more
than half of it. The reduced-depth arms land 3–5% below the
lossless one here, as the per-stream CPU table said they would — they buy bytes,
not capacity.

UberSDR-IQ also scales the way a per-stream codec should: 5.8× from one thread
to sixteen, on eight physical cores, because each stream's predictor state is
private and the work is compute-bound. SDR++'s plain convert modes stop scaling
earlier — at 48 kHz float32, int16 and int8 all converge on roughly 100,000
streams from four threads onward, which is a memory-bandwidth ceiling rather
than a CPU one, so their single-thread headroom does not multiply out. The
384 kHz vectors are 115 kB and stay resident in cache even in sixteen copies, so
the convert rows there are the optimistic end of what that plateau allows.

### Reference machine, and what else it was doing

    AMD Ryzen 7 5700U, 8 cores / 16 threads, 4372 MHz max
    governor `performance`
    Linux 6.8, 2026-09-05, three full runs of ./run.sh between 13:16 and 13:34

`bench` prints the model, logical CPU count and the mean clock sampled while it
ran, so figures from another machine carry the context needed to compare them.
Timings are averaged over repeated passes — five on the 30 s capture, 200 on the
0.075 s vectors — and then over three whole runs of `run.sh`. The saturation
pass runs two seconds per thread count, and is most of `run.sh`'s runtime; both
are `bench` arguments, and passing 0 for the window skips saturation.

**Load conditions, stated because they matter.** This is a workstation, not a
quiet bench. One-minute load average immediately before run 1 was **1.18** on 16
threads — a browser, an editor and two idle `buildkit` daemons, no build in
progress — and runs 2 and 3 started at 5.06 and 5.08 with the previous run's
threads still draining. Nothing else was scheduled against the machine during
the window. The statistic quoted throughout is the **mean of the three runs**,
with the min–max range shown wherever a number is load-bearing; nothing here is
a best-of.

The variance is real and mostly clock, not contention. This is a mobile part
whose clock moves under thermal and power limits: the mean clock during the
single-threaded 48 kHz pass ranged **1328–2502 MHz** across the three runs,
which is what puts a 10% spread on the per-stream CPU figures. Under all-core
saturation the clock is far steadier, 2345–2421 MHz, and the saturation figures
move by 3–12% for the UberSDR-IQ rows and up to 34% for the zstd rows.
UberSDR-IQ's 48 kHz cost measured between 0.379% and 0.419% of a core, and its saturated
48 kHz capacity between 1354 and 1428 concurrent streams. The ratios between
modes are far steadier than the absolute numbers, and it is the ratios the
tables are for. A desktop or server part, which is what a receiver of this kind
normally runs on, holds a higher all-core clock than this one does.

## Summary

| dimension | winner | by how much |
|---|---|---|
| Bytes, lossless against lossless | **UberSDR-IQ** | 1.47× smaller than a magnitude-scaled int16, 2.9× smaller than the float32 mode SDR++ ships |
| Bytes, lossy against lossy, 48 kHz | **UberSDR-IQ** | 1.30× smaller than int8 at the default 26 dB margin, and better than it on every fidelity axis at the same time; 1.85× smaller at 15 dB, trading 5.6 dB of SNR for it |
| Bytes, lossy against lossy, medium wave | SDR++ int8 | 0.95% smaller than our 15 dB arm, at a 3.0 dB margin against 35.6 dB |
| Fidelity, lossless | **UberSDR-IQ** | exact on every capture; nothing to tune, nothing to check |
| Fidelity, lossy | **UberSDR-IQ** | holds a stated margin on every capture and clips nothing; int8 clips 4432 samples on the 48 kHz capture and promises no margin at all |
| Adapts to the signal | **UberSDR-IQ** | 1.448×–1.583× lossless as the band changes, 1.745×–3.607× reduced-depth, against a fixed 0.989× or 1.957× |
| Noise floor kept, medium wave | **UberSDR-IQ** | 0.71–1.78 dB under requantisation; 0.00 dB under a margin request |
| Scales across cores | **UberSDR-IQ** | 5.8× on 8 cores / 16 threads; the convert modes hit a memory ceiling at four |
| Whole-machine capacity | ample either way | 1380 concurrent 48 kHz or 167–180 concurrent 384 kHz sessions, measured saturated |
| CPU | SDR++ | 13.8 CPU-s/h cheaper per 48 kHz session — 0.39% of a core against 0.011% |
| Tolerates packet loss | SDR++ | per-block independence, though both run over TCP |

Carrying the receiver's samples intact, UberSDR-IQ sends 132.6 kB/s where
`sdrpp_server`'s lossless mode sends 386.4 kB/s and a magnitude-scaled int16
sends 194.4 — 914 MB and 222 MB less per session-hour. At 384 kHz the margin is
2.1 GB per session-hour. Its ratio improves as the band gets more structured,
where requantisation returns the same fixed number whatever it is given, and
SPEC.md §10.1 records 60× on a squelched channel.

Where a client will take an approximation, the concession this document used to
make to int8 no longer holds on either count. At the reference sender's default
margin UberSDR-IQ sends 1.30× fewer bytes on the 48 kHz capture and 1.31× fewer
on the busy 384 kHz band, and it does so while holding a margin the client asked
for and clipping nothing, where int8 rails 4432 samples and offers no promise
about what survives. Medium wave is the exception and int8 keeps it by 0.95%,
because carriers 67.6 dB over the noise genuinely need the depth.

Three things go the other way, and all are bounded. UberSDR-IQ costs 0.39% of
one core per 48 kHz session and 3.1–3.4% per 384 kHz session — 14 CPU-seconds
and about two CPU-minutes per hour respectively, against 0.4 and 2.7 seconds for
a requantising convert; the reduced-depth mode costs the same again, so it buys
bytes and not cycles. Saturated, that still leaves this eight-core laptop part
serving 1380 concurrent 48 kHz sessions or 167–180 concurrent 384 kHz sessions,
so on any real receiver the cost is one a deployment can pay without noticing.
And its predictor is backward adaptive, so a stream needs every packet in order
from the first (SPEC.md §8.2), where each SDR++ block decodes alone. Both
systems stream over TCP, so neither exercises that difference today.

---

This file is licensed under CC BY-SA 4.0 (`../LICENSE-DOCS`).
