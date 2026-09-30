# UberSDR-IQ

A lossless compression format for software-defined radio sample streams:
complex baseband IQ and demodulated audio, as 16-bit integers.

This document specifies the format completely. It is written so that a reader
can implement an encoder and a decoder from it alone, and so that the reasoning
behind each decision is recoverable rather than having to be guessed at. Every
performance figure in it was measured; the measurements are reproducible with
the tools and vectors in this repository.

**Licence.** Copyright (C) 2026 Nathan Higgins. This document is licensed under
[Creative Commons Attribution-ShareAlike 4.0 International](https://creativecommons.org/licenses/by-sa/4.0/) (CC BY-SA 4.0); the
full text is in `LICENSE-DOCS`. Copy it, translate it and build on it,
including commercially, keeping the attribution and licensing derived
text alike. The reference implementations that accompany it are separately
licensed under AGPL-3.0-or-later (`LICENSE`). Neither licence restricts
implementing the format: an encoder or decoder written from this document
is your own work, under whatever licence you choose.

---

## 1. Scope

The format compresses streams of 16-bit signed integer samples that come from a
radio receiver:

- **Complex baseband (IQ)**, interleaved as I, Q, I, Q, at rates from a few
  kilohertz to several hundred kilohertz.
- **Demodulated audio**, mono, typically 12 or 24 kHz.

It is lossless: the samples that come out are bit-for-bit the samples that went
in. It is a streaming format with no framing of its own, designed to be carried
one packet per message on a reliable ordered transport, or written to a file
with a length prefix.

One profile is the exception, and is labelled as such on every packet that uses
it. Profile 2 (§6.6) requantises IQ samples by a power of two before coding
them, for a client that has asked for a reduced depth; the codec is still exact
over what it is given, and the scale travels in the packet. A stream that never
carries profile 2 is lossless end to end, and a decoder cannot be handed
profile 2 by a sender it did not ask.

It is not a general-purpose compressor and does not try to be. It exploits one
specific property — that consecutive samples of a band-limited signal are
correlated — and has a defined fallback for input that does not have it.

**Status.** The format is deployed and stable. The values in §4.2 (flag bits),
§6.1 (profile ids) and §5.2 (the magic) are fixed; changing any of them makes a
different format. So are the arithmetic constants of §6.2 — the tap scale, the
tap bound and the two leak shifts — because both ends must compute the same
filter state from the same bytes. Everything in §12.2 may be varied by an
implementation.

---

## 2. Why a special-purpose format

General-purpose compressors do not work on this data, and the reason is
structural rather than a matter of tuning.

zstd, deflate and their relatives are LZ77 matchers over bytes: they look for
repeated byte strings. A band-limited RF signal has none. Measured over the
reference captures, zstd returns:

| input | zstd ratio |
|---|---|
| IQ, 12 kHz | 0.985 |
| IQ, 384 kHz | 0.990 |
| demodulated audio | 0.970 |

Every figure is below 1.0: the compressed stream is consistently **larger** than
the samples it carries. The compressor correctly concludes the data is
incompressible and emits a stored block, paying a frame header for nothing. Over
one run of 32,179 consecutive IQ packets, not one was made smaller. The wasted
CPU is a match search that could only ever return that answer.

"No repeated strings" is not "no redundancy". The redundancy in this data is
between neighbouring samples, and it is substantial:

| capture | lag-1 correlation |
|---|---|
| IQ 384 kHz, empty band | 0.034 |
| IQ 384 kHz, medium wave carriers | 0.239 |
| IQ 384 kHz, busy amateur band | 0.582 |
| IQ 12 kHz | 0.906 |
| demodulated SSB voice | 0.701 |
| demodulated SSB data | 0.776 |
| demodulated NFM | 0.876 |
| demodulated CW | 0.965 |
| demodulated AM | 0.994 |

All measured as `|E[x[n]·conj(x[n-1])]| / E[|x[n]|²]`, which for a real signal
is the ordinary normalised lag-1 autocorrelation and for a complex one is the
magnitude of its complex equivalent. It is the quantity a zero-mean predictor
actually exploits, and it is not mean-removed; for the two captures with a
noticeable DC component the mean-removed Pearson figure is lower (NFM 0.550,
AM 0.976).

A predictor turns that correlation into small residuals and an entropy coder
suited to residuals turns small residuals into few bits. That is what this
format is.

---

## 3. Overview

A stream is a sequence of **packets**. Each packet carries one block of samples
and consists of:

```
[header][body]
```

The **header** (§5) is a metadata record — timestamp, sample rate, channel
count, sample count, optional signal-quality readings — that transmits only the
fields which have changed since the previous packet. It averages 7.7 bytes on
IQ and 9.1 on audio.

The **body** (§6) is the samples, in one of three forms selected by flags in the
header:

| form | body |
|---|---|
| coded | one byte of Rice parameter, then a Rice bitstream of prediction residuals |
| escape | the samples verbatim, as little-endian int16 |
| silent | nothing at all; the samples are known to be zero |

On the one scaled profile (§6.6) a coded or escaped body carries one further
byte in front of all of that — the shift the samples were requantised by. A
silent body does not, because there is no body.

The predictor is **backward adaptive**: its coefficients are a function of
samples that have already been coded, so the decoder recomputes them from its
own reconstruction and no coefficients are ever transmitted. Both ends run the
same integer arithmetic over the same samples and therefore hold identical
filter state, packet after packet, with nothing exchanged about it.

---

## 4. Conventions

### 4.1 Notation

- All multi-byte integers on the wire are **little-endian**.
- `uvarint` is the unsigned LEB128 varint: seven bits per byte, least
  significant group first, high bit set on every byte but the last.
- `varint` is a signed value zigzag-mapped to unsigned (`(n << 1) ^ (n >> 63)`
  for a 64-bit `n`) and then written as a `uvarint`, so a small negative value
  still costs one byte.
- `int16`, `int32`, `int64`, `uint32`, `uint64` are two's-complement integers of
  that width.
- A **sample** is one `int16`. A **frame** is one sample per channel: one
  `int16` for mono, an I/Q pair for two channels. `sample_count` in the header
  counts samples, not frames.

### 4.2 Flag bits

One byte in the header carries all of the following. The values are fixed.

| bit | mask | name | meaning |
|---|---|---|---|
| 7 | `0x80` | escape | the body holds verbatim samples |
| 6 | `0x40` | quality | the header carries the signal-quality pair |
| 5 | `0x20` | metadata | resynchronisation point: full timestamp, sample rate and channel count present |
| 4 | `0x10` | silent | every sample is zero; there is no body |
| 3 | `0x08` | count | the header carries the sample count |
| 2–0 | `0x07` | profile | payload codec profile id |

`escape` and `silent` are mutually exclusive; a packet with both set is invalid.

---

## 5. The header

### 5.1 Layout

```
offset  size      field                      present
------  --------  -------------------------  -----------------------------
0       4         magic = 0x344D4350         always
4       1         flags                      always
5       8 or 1-10 timestamp                  always (form depends on metadata)
        1-10      sample_count (uvarint)     if count
        1-10      sample_rate (uvarint)      if metadata
        1         channels                   if metadata
        2         baseband_power (int16)     if quality
        2         noise (int16)              if quality
```

Fields appear in exactly this order. The body follows immediately; on the
scaled profile of §6.6 its first byte is the shift.

The header does not state its own length or the body's. On a message transport
the frame boundary gives the packet length; in a file the container's length
prefix does (§9). A decoder derives the body length by subtracting the header
length it just parsed from the packet length.

### 5.2 Magic

`0x344D4350`, which on the wire is the four bytes `50 43 4D 34` — the ASCII string `PCM4`,
retained from the format's original name. It carries no meaning beyond being a
constant, and is chosen for being visible in a hex dump.

Four bytes is a deliberate choice rather than an inherited one. In the
deployment this format comes from, these packets share a socket with lossy
frames of another kind that carry no magic of their own, because the sender
selects the codec per packet. Under those conditions the width of a magic is a
false-positive rate, and each false positive corrupts one block of audio:

| width | false positive rate | at 1099 packets/s |
|---|---|---|
| 1 byte | 1 in 256 | about 4 per second |
| 2 bytes | 1 in 65,536 | one per minute |
| 4 bytes | 1 in 4.29 × 10⁹ | one per several years |

Two bytes looks sufficient until the rate is worked out. Four bytes against a
packet of roughly a kilobyte costs nothing measurable.

### 5.3 Timestamp

The time of the **first sample in the packet**, in nanoseconds. The format does
not specify an epoch or a clock; it requires only that the value advances
monotonically within a stream. In the reference deployment it is a
GPS-disciplined Unix-epoch nanosecond count.

- At a resynchronisation point (metadata flag set) it is a full `uint64`, 8
  bytes.
- Otherwise it is a `varint` delta from the previous packet's timestamp.

Deltas are not uniform. A receiver delivers in bursts, and gaps measured on live
traffic ranged from 2 microseconds to 28 milliseconds. A varint absorbs that:
it averages 2.5 bytes on IQ and 4 on audio and is never worse than the 8 an
absolute value costs.

### 5.4 Sample count

The number of `int16` samples in the body, counting both channels of an
interleaved IQ frame. Present when the count flag is set; otherwise the value
from the previous packet carries forward.

This field is not redundant. With an uncompressed body the packet length would
give the count away; with a coded body there is no such relationship — that is
what compression means — and the decoder must know how many samples to produce
before it can run the predictor.

Nor can the count be assumed constant. A real receiver does not deliver a fixed
block size: across the reference captures a 384 kHz stream carries 720 samples
on 95% of packets and 240 on the rest. Tracking and retransmitting the count
costs about 0.18 bytes per packet on IQ and effectively nothing on audio.

### 5.5 Sample rate and channel count

Present only at a resynchronisation point. `sample_rate` is a `uvarint` in
hertz; `channels` is one byte, 1 for mono and 2 for interleaved I/Q. A decoder
must reject a packet that gives a rate or channel count of zero.

Both are carried forward between resynchronisation points.

### 5.6 Signal quality

Two `int16` values, present when the quality flag is set and carried forward
otherwise. They are passengers: nothing in the codec reads them, and a stream
with no such readings sets both to the "no reading" codepoint once and never
sends them again.

The unit is **signed centidecibels** — 0.01 dB per step. `-32768` is reserved
for "no reading" and maps to the -999 sentinel that receiver APIs conventionally
use; -999 dB cannot itself be represented, because -99900 overflows an `int16`.
Finite values outside ±327.67 dB are clamped. A value that is not a number is
not a reading and maps to the reserved codepoint.

0.01 dB keeps the error in a derived signal-to-noise ratio below 0.01 dB, which
is roughly twenty times finer than a receiver UI displays. Two bytes rather than
four costs nothing measurable because the pair is only transmitted when it
changes — 1% of packets on a 384 kHz IQ stream and 20% on audio. One byte would
put half a decibel into the SNR, which a one-decimal readout would show.

### 5.7 Resynchronisation

A packet is a **resynchronisation point** when the metadata flag is set. Such a
packet carries a full timestamp, the sample rate and the channel count, and (by
the rule below) the quality pair and the sample count, and therefore describes
itself completely without reference to any earlier packet.

An encoder MUST make a packet a resynchronisation point when:

1. it is the first packet of the stream; or
2. the sample rate or channel count differs from the previous packet; or
3. the timestamp moves backwards, or forward by more than the resync interval —
   the stream is not continuous where a delta would assume it was.

An encoder SHOULD additionally emit one every **resync interval** of stream
time. The reference value is 5 seconds, which costs about 0.02 bytes per packet.

An encoder MUST set the quality flag on every resynchronisation point, so that
such a packet is genuinely self-describing, and SHOULD otherwise set it only
when a reading has changed. Likewise for the count flag.

**What resynchronisation does and does not do.** It restores the *header* state:
a reader that opens a stored stream at an arbitrary offset learns the sample
rate, channel count and timestamp at the next one. It does **not** restore
predictor state, which is never transmitted. See §8.3.

### 5.8 Sizes

Measured, and derivable from the layout:

| case | size |
|---|---|
| steady state, nothing changed | 8 bytes (magic 4, flags 1, delta 3) |
| resynchronisation, 384 kHz stereo, 720 samples | 23 bytes |
| resynchronisation, widest plausible rate and count | 25 bytes |
| theoretical maximum | 36 bytes (three 10-byte varints) |
| mean over a 384 kHz IQ capture | 7.66 bytes |
| mean over a 12 kHz audio capture | 9.06 bytes |

A fixed record carrying the same information costs 37 bytes on every packet.

### 5.9 Decoder state

A header decoder carries, between packets: whether metadata has been seen at
all, the last timestamp, the sample rate, the channel count, the sample count
and the two quality readings.

A decoder MUST reject a packet that is not a resynchronisation point if it has
not yet seen one, rather than guessing at the missing fields. That is the normal
state for a reader that opened a stored stream part-way through, and §5.7 is
what ends it.

---

## 6. The payload codec

### 6.1 Profiles

The low three bits of the flags byte name the predictor configuration the body
was coded with. Three are defined:

| id | name | form | cascade (order, step) |
|---|---|---|---|
| 0 | `iq-complex-o16` | complex | one stage: order 16, µ = 16 |
| 1 | `audio-real-8/8/4/2` | real | four stages: 8/µ16, 8/µ16, 4/µ32, 2/µ32 |
| 2 | `iq-complex-o16-scaled` | complex | one stage: order 16, µ = 16 |

Profile 2 is profile 0's predictor, unchanged, over requantised samples: its
body carries a leading shift byte and its output is scaled back up. §6.6
specifies it. Everything else in §6 applies to all three profiles alike.

Ids 3–7 are unassigned. A decoder encountering one MUST fail. It MUST NOT fall
back to profile 0: decoding with the wrong predictor does not produce an error,
it produces plausible-sounding noise, which is the worst possible outcome for a
format whose entire promise is bit-exactness. A decoder that does not implement
profile 2 MUST refuse it on the same terms as an unassigned id — which is the
whole reason the reduced-depth mode is a profile rather than a flag bit (§6.6).

**The packet declares its profile; a decoder never infers one.** It does not
look at the sample rate, the channel count or anything else to choose a
predictor. Three bits in a byte the header already had make self-description
free, and the consequence is that predictor selection stays entirely a sender
policy question. A sender may change how it chooses — per band, per mode, per
measurement — and every deployed decoder keeps working. Inferring the predictor
from the channel count would have frozen one particular choice into every
decoder permanently.

The reference sender's policy is simply: two or more channels selects profile 0,
otherwise profile 1; and profile 2 in place of profile 0 while a client has
asked for a reduced depth. Nothing on the wire depends on that being the policy.

That this matters is not hypothetical. On the reference captures, profile 0
beats a deep real cascade on two of the three IQ streams and loses to it on the
third:

| IQ capture | profile 0 (complex, order 16) | two independent real 8/8/4/2 cascades |
|---|---|---|
| 12 kHz | **1.447** | 1.382 |
| 384 kHz, busy band | **1.598** | 1.528 |
| 384 kHz, medium wave carriers | 1.589 | **1.703** |

(Payload ratios on the repository's vectors, which are short enough that a cold
predictor still costs a few percent — see §14. The I and Q channels of the
deinterleaved case measured within 0.01 of each other; the higher is shown.
Ratios in this section and in §12.2 are comparable with each other but lower
than the steady-state figures in §10.1.)

A carrier-dense band has structure at several scales, which is what a cascade
removes; a signal spread across the band does not, and there the complex form's
ability to cancel a rotating phasor with a single tap wins. Which predictor is
best is a property of the signal, and the format is arranged so that discovering
that later costs nothing.

### 6.2 The predictor

Each sample is predicted from the `order` samples before it by an FIR filter
whose taps adapt as the stream runs. Only the prediction error — the
**residual** — is coded.

**All arithmetic is integer.** No step below may be performed in floating point
unless the implementation can guarantee identical results, because both ends
must compute bit-identical filter state from bit-identical inputs. (A float64
implementation *is* exact here, and that is not an accident — see §6.2.6.)

#### 6.2.1 State

Per stage:

- `w[0..order-1]`: taps, integers in **Q16** — 65536 represents 1.0. Initially
  zero. For a complex stage there are two arrays, `wr` and `wi`.
- `h[...]`: history of reconstructed samples, initially zero, and their signs.
  For a complex stage, `hr` and `hi`.

The history need only be `order` deep. Implementations in this repository keep a
longer linear window and slide it periodically, so the tap loop walks contiguous
memory with no index wrapping; a circular buffer is equally valid and changes
nothing that is transmitted.

#### 6.2.2 Rounding

```
roundShift(v, s) = round-to-nearest, ties away from zero, of v / 2^s
```

Explicitly, for `s = 16`:

```
if v < 0:  -(( -v + 32768) >> 16)
else:       (( v + 32768) >> 16)
```

A plain arithmetic right shift rounds negative values towards negative infinity,
which biases the predictor — and, decisively, both ends must round the same way.
This is the single definition every implementation copies.

#### 6.2.3 Tap leakage

Every tap update subtracts a fraction of the tap from itself before the gradient
step is added:

```
leak(w, s) = the magnitude of w divided by 2^s, truncated towards zero,
             with the sign of w restored

Explicitly:  if w < 0:  -(( -w) >> s)
             else:       (( w) >> s)
```

with

```
s = 14   for a complex stage
s = 16   for a real stage
```

Truncation, not rounding, and towards zero rather than towards negative
infinity. A tap smaller in magnitude than `2^s` therefore leaks **nothing**,
which is the point: a tap the signal genuinely wants at a small value stays
there instead of being pulled towards zero and pushed back on every sample. A
plain arithmetic right shift is wrong for the same reason it is wrong in §6.2.2,
and in a language whose `>>` is 32-bit it is wrong twice over, because taps
reach past 2^31.

**The leak is not optional and its absence is silent.** Sign-sign LMS has no
restoring force of its own: the step is `mu·sgn(e)·sgn(hist)` whatever the taps
already are, so in any direction the input does not excite — and a band whose
energy sits in a few carriers leaves most directions unexcited — the taps are
free to walk. Measured on a 909 kHz medium-wave carrier in a 384 kHz baseband,
the mean tap magnitude grew linearly at about 1.5 Q16 units per second until it
reached the clamp of §6.2.6 ninety seconds in. Long before the clamp the
prediction is worthless: that stream cost 10.65 bits per sample at five seconds,
14.26 at thirty and 15.85 at ninety, by which point a third of its packets were
taking the escape and the coded stream was larger than the samples going into
it. With the leak it holds 8.45 bits per sample for as long as it runs.

The two shifts differ because the two filters differ. 14 is the complex stage's
measured optimum on that stream — bits per sample over its last five seconds:

| leak shift | 8 | 10 | 12 | **14** | 16 | 18 | none |
|---|---|---|---|---|---|---|---|
| bits/sample | 13.35 | 9.41 | 8.81 | **8.45** | 9.36 | 11.19 | 15.85 |

The real cascade is four short stages over a 12 kHz channel about four times
oversampled, whose taps legitimately reach 1.2, and leaking it as hard throws
that away. On a 14.074 MHz USB capture, payload ratio against the largest tap
magnitude reached after 150 million samples:

| leak shift | 14 | **16** | 17 | none |
|---|---|---|---|---|
| ratio | 1.671 | **1.874** | 1.906 | 1.905 |
| largest tap | 0.90 | **1.57** | 1.99 | 5.33 |

16 and 17 are within 0.14% of each other summed over five audio captures, so the
choice between them is not a compression one. **16 is specified because it is
the one that can be tested.** A tap has to pass 1.0 before it leaks at all,
which every audio capture does within a second; at 17 nothing leaks until a
stream has run for tens of millions of samples. Had 17 been chosen, an
implementation that omitted the real cascade's leak entirely would reproduce
every vector in §14 and fail only on a live connection. At 16 it cannot: the
omission changes coded bytes inside the first few seconds.

That is the shape of this whole section, and it is worth stating plainly. **The
leak changes coded bytes early and buys its benefit late.** On the vectors of
§14 the first packet that differs with the leak removed is the 8th of `iq-12k`,
the 11th of `iq-384k-mw`, the 21st of `iq-384k`, the 44th of `audio-usb-12k` and
the 125th of `audio-cw-12k` — every one of them well inside a vector a few
seconds long. The *win*, meanwhile, needs minutes of a carrier-dense band, and
where the taps were not walking there is none to see: on two minutes of a 40 m
capture the leak changed the coded size by 0.003%, in its favour. So a
conformance run detects the leak reliably and demonstrates nothing about why it
is there, which is why the measurements above are in the specification rather
than left to the vectors.

#### 6.2.4 Prediction and update, real form

For each sample `x`, with `hist[j]` the history sample the tap `w[j]` weighs
(oldest first):

```
p = roundShift( sum over j of w[j] * hist[j], 16 )
e = x - p                       # encoder
x = e + p                       # decoder
```

then, with `sgn(v) ∈ {-1, 0, +1}`:

```
if e != 0:
    m = mu * sgn(e)
    for each j:  w[j] = clamp( w[j] + m * sgn(hist[j]) - leak(w[j], 16) )
```

then push `x` onto the history.

`leak` is computed from the tap's **old** value, subtracted in the same
expression as the gradient step, and the clamp is applied to the result of both.

This is **leaky sign-sign LMS**. It uses only the sign of the error and the sign
of each history sample, so the update needs no division, no normalisation and no
multiplication by a data value. Two consequences matter:

- It is exactly reproducible in integers, which is what makes cross-language
  bit-exactness achievable at all.
- The whole update, leak included, is skipped when the error is zero. **This is
  required, not an optimisation.** Prediction over an all-zero history is zero,
  so the error over digital silence is zero, and this is the branch that makes
  the taps **freeze** across a squelched run rather than leaking towards zero:
  whatever the filter had learned before the silence is still there when signal
  returns. An implementation that adds the leak but runs it on a zero error
  parts company with the encoder on the first silent packet.

Measured compression is within a fraction of a percent of a floating-point NLMS
of the same order.

#### 6.2.5 Prediction and update, complex form

Identical with the imaginary terms restored. With `x = xr + j·xi` and taps
`w = wr + j·wi`:

```
pr = roundShift( sum of  wr[j]*hr[j] - wi[j]*hi[j], 16 )
pi = roundShift( sum of  wr[j]*hi[j] + wi[j]*hr[j], 16 )
```

and the update uses the **conjugate** of the history, as the complex LMS
gradient requires — which here is just the negated sign of the imaginary part:

```
if er != 0 or ei != 0:
    mr = mu * sgn(er);   mi = mu * sgn(ei)
    for each j:
        hrs =  sgn(hr[j])
        his = -sgn(hi[j])
        wr[j] = clamp( wr[j] + mr*hrs - mi*his - leak(wr[j], 14) )
        wi[j] = clamp( wi[j] + mr*his + mi*hrs - leak(wi[j], 14) )
```

Each tap leaks from its own old value: `wr` from `wr` and `wi` from `wi`, not
from the magnitude of the pair. The shift is 14 here where the real form uses
16; §6.2.3 gives the measurements behind both.

The early return applies to the pair jointly: adaptation is skipped only when
**both** components of the error are zero, so a frame with a zero I and a
non-zero Q still adapts and still leaks.

The real form is exactly this with every imaginary term deleted; the two are one
algorithm and an implementation may write them as one.

#### 6.2.6 The tap clamp

```
clamp(w) = max(-2^24, min(2^24, w))
```

A magnitude of 2^24 in Q16 is a real-valued tap of 256, which is far outside
anything adaptation produces. The leak of §6.2.3 is what makes that true: it
bounds the walk at roughly `mu·2^s`, which is 2^18 for the complex stage and
2^20 or 2^21 for the real one, three orders of magnitude below the limit. The
clamp never fires in practice. It must nonetheless be applied identically on
both sides, because on the input where it does fire the two must agree.

Without the leak the clamp is not insurance but a destination: the medium-wave
stream of §6.2.3 reached it in ninety seconds. A tap bound is not a substitute
for a restoring force, which is why the format has both.

It is not arbitrary. It does two things:

1. It bounds the prediction sum far below `int64` overflow whatever the input
   does. An order-16 filter at the limit against full-scale samples reaches
   about 2^45.
2. **That same bound keeps every product and every partial sum inside 2^53**,
   the range in which an IEEE-754 double represents integers exactly. A
   JavaScript decoder can therefore use ordinary numbers and still be bit-exact,
   with no BigInt and no 64-bit emulation. The JavaScript decoder in this
   repository does exactly that and reproduces the reference vectors sample for
   sample.

**Clamp elision.** A conforming implementation may skip the clamp for a whole
packet when it can prove the clamp is an identity over that packet. One real
update moves a tap by at most `mu` and one complex update by at most `2·mu`, and
the leak only ever moves a tap towards zero, so it cannot invalidate the bound.
If, at the start of a packet of `n` update steps, every tap satisfies

```
|w| + mu·n  ≤  2^24        (real)
|w| + 2·mu·n ≤ 2^24        (complex)
```

then no update in that packet can reach the limit. This is an optimisation with
no effect on output — an implementation that always clamps produces identical
bytes — and it is described here only because implementations do it and a reader
comparing two of them will see it.

#### 6.2.7 The cascade

A profile may specify several stages. Each stage predicts the **residual left by
the stage before it**:

```
encode:   r = x;   for stage in stages:            r = stage.forward(r)
decode:   r = residual; for stage in reverse(stages): r = stage.inverse(r)
```

The decoder applies stages in **reverse order**: the last stage to have
predicted is the first to be undone. Getting this backwards is a
silently-wrong-output bug, not an error.

Depth is the dominant parameter for demodulated audio and nearly irrelevant for
wideband IQ. Measured on a demodulated SSB capture:

| cascade | ratio |
|---|---|
| one stage, order 16 | 1.344 |
| one stage, order 32 | 1.362 |
| 32 / 8 | 1.489 |
| 16 / 8 / 4 | 1.607 |
| **8 / 8 / 4 / 2** | **1.711** |
| 8 / 8 / 4 / 4 / 2 | 1.841 |

(Payload ratios on this repository's `audio-usb-12k` vector.) A 12 kHz channel
carrying a 2.65 kHz passband is about 4× oversampled and has
structure at several scales for successive stages to remove. Wideband IQ gains
nothing from the same treatment at roughly double the cost: against 1.396 for
one order-16 complex stage, 8/8/4/2 measured 1.391 and 16/8/4/2 measured 1.403.

### 6.3 Rice coding of residuals

The residuals of one packet are coded as a Rice (Golomb–Rice) bitstream.

#### 6.3.1 Parameter selection

```
k = 0                                    if mean == 0
k = min(30, bit_length(mean) - 1)        otherwise

where mean = (sum over all residuals of zigzag32(r)) / count,   integer division
      zigzag32(r) = (r << 1) ^ (r >> 31)   as unsigned 32-bit
```

`k` is written as the first byte of the body. A decoder MUST reject `k > 30`.

#### 6.3.2 Codeword

For each residual, in order, with `u = zigzag32(r)`:

```
q = u >> k                 high part
    q one-bits, then one zero stop bit
    then the low k bits of u, least significant first
```

#### 6.3.3 Bit order

Bits are written **least-significant-bit first within each byte**, and bytes in
increasing address order. The final byte is zero-padded in its unused high bits.

An encoder that buffers into a wider accumulator and flushes 32 bits at a time
as a little-endian `uint32` produces the same byte sequence as one that emits a
byte at a time; both appear in this repository.

Long unary runs are real and must be handled. A strong carrier against a low
noise floor produces residuals far above the mean, and `q` can exceed any fixed
accumulator width. An encoder MUST emit long runs in chunks rather than
assuming a codeword fits one accumulator write.

The decoder has the mirror of that problem, and it is sharper because it is
silent. A decoder that buffers bits in a 64-bit accumulator, refilling while it
holds 56 bits or fewer, can hold exactly 64; a 63-bit unary run and its stop bit
then consume all 64 at once. In C and C++ shifting a 64-bit value by 64 is
undefined behaviour, and on the common architectures the shift count is taken
modulo the width, so the accumulator keeps every bit it had and the next refill
merges new bytes into stale ones. A decoder MUST treat a shift of the full
accumulator width as producing zero, whatever its host language does.

Nothing reports it when this is wrong. The rest of the packet decodes to
plausible residuals, and because the predictor is backward adaptive (§7) those
wrong samples enter the filter state, so the stream stays wrong for the rest of
the connection rather than for one packet. Measured on live 96 kHz IQ the
condition arises roughly once in a quarter of a million packets: often enough to
break a receiver within minutes, rare enough that a captured test vector will
not contain one. The `rice-63` vector exists for it.

#### 6.3.4 Why Rice

Prediction residuals from this data really are close to Laplacian, and Rice is
near-optimal for a Laplacian source. Against the order-0 entropy of the
residuals themselves, computed over each whole capture, the Rice bitstream
costs:

| capture | residual entropy (bits/sample) | Rice (bits/sample) | ratio |
|---|---|---|---|
| IQ 384 kHz busy band | 10.750 | 10.742 | 0.999 |
| IQ 384 kHz medium wave | 8.669 | 8.454 | 0.975 |
| IQ 384 kHz quiet band | 10.332 | 10.313 | 0.998 |
| IQ 12 kHz | 11.321 | 11.345 | 1.002 |
| audio SSB data | 8.392 | 8.364 | 0.997 |
| audio SSB voice | 6.960 | 6.991 | 1.004 |
| audio CW | 3.575 | 3.594 | 1.005 |
| audio AM | 9.264 | 9.364 | 1.011 |
| audio NFM | 10.968 | 11.307 | 1.031 |

Within about 3% throughout, and below 1.0 in three cases because `k` adapts per
packet where the entropy figure is one global distribution. A more elaborate
entropy coder has almost nothing left to win. That was confirmed directly: a
full adaptive arithmetic/rANS model over the residual alphabet measured level
with this (1.35–1.39× either way) at far greater cost, adapting `k` continuously
rather than per packet was slightly worse, and FLAC-style partitioning changed
nothing. Unary accounts for only 16–24% of the coded bits, so there is no
escape-coding win hiding either.

### 6.4 The escape

If the coded body would be at least as large as the samples themselves, the
encoder discards it and writes the samples verbatim as little-endian `int16`,
setting the escape flag in the header.

```
if len(coded_body) >= 2 * sample_count:
    body  = samples as little-endian int16
    escape = true
```

The escape exists because a predictor cannot help a full-entropy signal, and a
saturated receiver front end produces exactly that. Without it, such a stream
would expand by about 3%. With it, **the body never expands**: the only growth
the format can produce is the header, bounded by §5.8.

Note that the profile and escape bits live in the header, where they are needed
anyway, so an escaped body carries no per-packet overhead at all — it is exactly
`2 × sample_count` bytes, or one more than that on the scaled profile, which
prepends its shift to every body it sends (§6.6.1). The comparison above is made
against the coded body alone; the shift byte rides on either outcome and does
not change which one is chosen.

**Both ends still run the predictor across an escaped packet**, over the
verbatim samples, discarding the residuals. If they did not, filter state would
diverge at the first escape and every subsequent packet would decode to noise.

Escapes are a safety valve, not a routine occurrence: none of the nine live
captures in the reference set takes the escape even once, while synthetic
full-scale white noise takes it on every packet.

### 6.5 Silence

If every sample in the packet is zero, the encoder sets the silent flag and
**writes no body at all**. A decoder receiving a silent packet MUST verify that
the packet ends where the header ended, produce `sample_count` zeros, and
advance its predictor over those zeros.

The reason is arithmetic. A zero residual Rice-codes to a bare stop bit, so
coded silence costs one bit per sample however well the predictor does — 30
bytes for a 240-sample packet. That is structural, not waste, and the only way
past it is to not send the body. A squelched receiver produces nothing but zeros
indefinitely, so this is not an edge case; it takes a 1440-byte block of silence
down to an 8-byte packet.

**Both ends still run the predictor across a silent packet**, over the implied
zeros, for the same reason as an escape. Because a sign-sign update on a zero
error skips adaptation entirely (§6.2.3), doing so costs almost nothing and
leaves the taps
exactly as they were.

An encoder detects silence by scanning the samples rather than being told about
it, which costs 0.18 µs on a 480-byte packet and 0.52 µs on a 1440-byte one —
under 2% of the roughly 30 µs it takes to encode one — and catches every source
of digital silence rather than only the one the caller knew about. The scan is
no more expensive when the first non-zero sample is the last one, so no input
makes it pathological.

### 6.6 Reduced-depth IQ: profile 2

Profile 2, `iq-complex-o16-scaled`, is profile 0's predictor over samples that
have been requantised by a power of two. It exists for an IQ client that does
not need the bottom bits of every sample and would rather not pay for them: the
codec is untouched, and the saving comes entirely from the numbers going into it
being smaller, which shortens every Rice codeword.

#### 6.6.1 What is on the wire

Everything in §6.2 through §6.5 applies unchanged. The only differences are:

1. The profile field is `2`.
2. A body that exists — coded or escaped — begins with **one extra byte, the
   shift**, before the Rice parameter or the verbatim samples.
3. A **silent** packet carries no shift byte, because it carries no body. The
   packet ends where the header ended, exactly as for profile 0. A decoder MUST
   test the silent flag before attempting to read a shift.
4. The shift is `0`–`15`. A decoder MUST reject a larger value, and MUST reject
   a non-silent profile-2 packet with no body at all.

So the three body forms become:

| form | body |
|---|---|
| coded | shift, then one byte of Rice parameter, then the bitstream |
| escape | shift, then the samples verbatim as little-endian int16 |
| silent | nothing at all |

The shift is in the body rather than the header because the flags byte is full,
and because putting it there costs a squelched or dead channel nothing.

#### 6.6.2 What the two ends do with it

The sender requantises, rounding to nearest and clamping to the depth the shift
implies:

```
q = (v + 2^(shift-1)) >> shift,  clamped to [ -32768 >> shift, 32767 >> shift ]
```

and hands the result to the codec. **The predictor, on both sides, runs on those
requantised values.** The decoder undoes the scale only on the way out, after
its predictor has produced its samples:

```
restore(v) = clamp_int16( v << shift )
           = min(32767, max(-32768, v << shift))
```

Order is the whole of it. Quantising before the encoder's predictor and
restoring after the decoder's is what keeps the two ends computing the same
filter state; a decoder that restored before running its filters would diverge
on its first packet. An implementation that applies a gain at its output anyway
may fold `2^shift` into that gain instead of shifting, which is the same thing.

Rounding rather than truncating in the quantiser matters: at ten bits, half a
step of the coarse grid is not a rounding detail, it is 42 dB below the peak.
Clamping matters because rounding can carry a full-scale peak one step past the
ceiling. The saturation in `restore` is the mirror of that clamp; a conforming
encoder never produces a value that needs it, which is exactly why it must be
specified rather than left to whatever an implementation's shift happens to do
with an out-of-range input.

A shift of `0` is legal and codes the samples exactly. A sender that wants a
lossless stream should nonetheless use profile 0 rather than declaring a lossy
profile and shifting by nothing, so that an archival stream is honestly
labelled.

#### 6.6.3 Why a profile and not a flag

A decoder that never asked for a reduced depth must not be able to be handed one
by accident. An unrecognised profile id is a hard error everywhere (§6.1), where
an unrecognised flag bit might be ignored — and a stream silently decoded
`6·shift` dB low is worse than a stream that fails. Making it a profile id also means the
choice is visible per packet, so a sender may cross between lossless and reduced
depth mid-stream; the profile change rebuilds the codec on both sides (§8.4) and
nothing else is needed.

The corollary is that a decoder MUST NOT treat profile 2 as profile 0 with an
extra byte to skip. If it does not implement the restore, it must refuse the
profile.

#### 6.6.4 Choosing the shift is not part of this format

Only the shift is on the wire, and a shift is not a policy. Nothing a decoder
does depends on reproducing the sender's choice, so a sender may re-derive it
per packet, per band, or from readings taken off its own receiver, and no
deployed decoder changes.

For orientation, the reference sender does not let a client ask for a bit depth
at all — it asks for a **margin**, in decibels, being how far below the band's
own noise floor the quantisation floor must be held, and the depth that honours
it is worked out per packet from the packet's crest factor and the band's
spectral peakiness. A depth cannot be promised to anyone because what it costs
depends entirely on the signal: ten bits leaves 50 dB of headroom on a quiet
6 m band and 9 dB on medium wave. A margin means the same thing on every band,
which is what lets a waterfall, a skimmer or an external SDR program reason
about the stream without knowing what it is tuned to. At a 20 dB request the
saving measured 60.6% on a quiet 6 m band, 59.9% on 40 m at 192 kHz, 50.3% on
20 m at 48 kHz and 15.6% on medium wave — medium wave spending the bytes because
its carriers sit 60 dB over the noise and it genuinely needs the depth.

How a client *asks* for that margin is a matter for the transport, not for this
format. In the deployment this format comes from it is a `min_margin` query
parameter on the streaming URL — an integer number of decibels, absent or `0`
meaning lossless, values outside 10–60 clamped rather than refused — and a
control message of the same name to change it on a live connection. Those names
and rules are documented with that transport; a stream carrying profile 2 says
everything a decoder needs, whatever asked for it.

### 6.7 Decoder procedure

```
1. Parse the header (§5). Reject if it fails.
2. If the declared profile differs from the codec in hand, build a new codec
   for it, discarding the old. Reject an unknown profile.
3. If silent:   verify there is no body; emit sample_count zeros;
                advance the predictor over sample_count zeros; done.
                (No shift byte is read, whatever the profile.)
4. If the profile is scaled: read the shift from the first body byte; reject an
                empty body; reject shift > 15; the body continues after it.
5. If escape:   verify the remaining body is at least 2*sample_count bytes; read
                the samples; advance the predictor over them; go to 7.
6. Otherwise:   read k from the next body byte; decode sample_count residuals
                from the bitstream; run the cascade in reverse over each
                sample position to reconstruct the samples.
7. If the profile is scaled: left-shift every sample by the shift, saturating to
                int16 (§6.6.2).
```

---

## 7. Encoder procedure

```
1. Choose the profile. If it differs from the codec in hand, build a new codec
   (§8.4).
2. For a complex profile, drop a trailing half-frame if the input has one.
3. For a scaled profile, choose a shift in 0..15 and requantise the samples by
   it (§6.6.2). Everything after this point works on the requantised values.
4. If every sample is zero: advance the predictor over them, emit a header with
   the silent flag and no body — and no shift byte.
5. Otherwise run the cascade forward over every sample position to produce
   residuals, Rice-code them, and take the escape if the result is not smaller
   than the samples (§6.4).
6. Emit the header with the resulting profile, escape and silent flags, and
   whichever fields have changed (§5.7); then, for a scaled profile on a
   non-silent packet, the shift byte; then the body.
```

An encoder has freedom the decoder does not: it may resynchronise more often
than required, choose a different `k`, take the escape earlier, choose any shift
it likes, or emit fields that have not changed. All of these produce a stream a
conforming decoder reads correctly.

It has no freedom at all in the predictor. The rounding rule, the leak, the
zero-error skip, the sign convention and the clamp are all shared with the
decoder, and an encoder that differs from this specification in any of them
produces a stream that decodes to noise without any error being raised.

---

## 8. Stream rules

### 8.1 A codec instance is a stream

Encoder and decoder state — filter taps, history, and the header's
carried-forward fields — belongs to exactly one stream. It must be created when
the stream starts, used by one thread, and discarded when the stream ends. It
must not be pooled or shared between streams, which is a real difference from
a general-purpose compressor whose state is meaningless between calls.

### 8.2 Every packet, in order, from the first

A decoder must be given every packet of the stream, in order, beginning with the
first one the encoder produced. A packet that never reaches the decoder — even
one whose samples the application intends to discard — leaves the decoder's
filters where the encoder's no longer are, and every subsequent packet decodes
to noise.

This means the format requires an **ordered, lossless transport**: a WebSocket
or TCP connection, a pipe, or a file. It is not usable over a datagram transport
without a retransmission layer.

Packets dropped *before* the encoder are harmless: both ends then see the same
sequence and simply re-adapt across the gap.

### 8.3 Random access

**A stream is not randomly accessible, and this is a property of the design.**

The periodic resynchronisation of §5.7 restores the header state only. Predictor
state is never transmitted and never reset mid-stream, so a decoder that begins
at an arbitrary packet has zero taps where the encoder has adapted ones, and
`x = e + p` is wrong from the first sample. It stays wrong: the incorrect
reconstruction enters the history, and the two sides never reconverge.

This is measurable rather than theoretical. Starting a decoder at packet 300 of
a 600-packet capture:

| capture | samples wrong | first correct packet |
|---|---|---|
| demodulated SSB | 71,999 of 72,000 (100.0%) | none |
| IQ 384 kHz | 209,274 of 209,280 (100.0%) | none |

(The handful of "correct" samples are coincidences.)

The alternative — periodically resetting the predictor to give true keyframes —
was rejected on measurement. A cold start costs, against a warm codec on
identical packets:

| capture | first packet | packet 10 | packet 25 | packet 100 | over 300 packets |
|---|---|---|---|---|---|
| IQ 384 kHz, busy band | +2.8% | −0.1% | −0.1% | 0.0% | **+0.04%** |
| IQ 384 kHz, medium wave | +58.6% | +26.4% | +1.9% | +7.7% | **+6.0%** |
| demodulated SSB data | +74.7% | +51.0% | +34.2% | +14.6% | **+14.7%** |
| demodulated SSB voice | +66.3% | +49.3% | +32.5% | +13.7% | **+13.3%** |
| demodulated CW | +213.0% | +80.6% | +26.0% | +4.3% | **+10.0%** |
| demodulated AM | +42.6% | −1.0% | −4.1% | −0.2% | **+0.13%** |

Wideband IQ converges within about three packets and would pay almost nothing
for keyframes. Narrowband audio takes one to two seconds and would pay 10–15% of
the whole gain for them. Since the transport this format was designed for is
ordered and reliable and delivers every stream from its first packet, keyframes
would have been a permanent cost for a capability nothing needed.

An application that does need random access into stored data should cut the
input into independent streams at chosen boundaries and index them, paying one
cold start per entry point — which is a container decision, not a format one.

### 8.4 Profile changes

A packet may declare a different profile from the one before it. A decoder MUST
then discard its codec and build a new one for the declared profile, starting
from zero state. An encoder that changes profile does the same.

Discarding the adaptation is correct rather than merely acceptable: a profile
change means the signal itself has changed — a different mode, a different
channel count — and taps trained on the old signal would predict the new one
worse than a cold start.

---

## 9. Stream files (informative)

The packet format has no framing, because on a message transport the frame
boundary supplies it. Storing packets in a file needs that boundary back, so
this repository defines the minimum that does:

```
[length uint32 little-endian][packet bytes]   repeated to end of file
```

No file header, no index, conventional extension `.uiq`. This container is a
convenience for tools, conformance vectors and examples. An implementation that
carries packets some other way is still speaking UberSDR-IQ.

---

## 10. Measured performance

All figures from a live receiver, nine captures of 600 consecutive packets
covering both lossless profiles, every sample rate offered, and signals from
nearly
incompressible to highly compressible.

### 10.1 Compression

Whole-packet, header included, against raw `int16`:

| stream | rate | raw | coded | ratio | saved |
|---|---|---|---|---|---|
| CW | 12 kHz audio | 24.0 kB/s | 5.8 kB/s | 4.11× | 76% |
| SSB voice | 12 kHz audio | 24.0 kB/s | 10.9 kB/s | 2.19× | 54% |
| SSB data | 12 kHz audio | 24.0 kB/s | 13.0 kB/s | 1.85× | 46% |
| AM | 24 kHz audio | 48.0 kB/s | 28.5 kB/s | 1.68× | 41% |
| NFM | 24 kHz audio | 48.0 kB/s | 34.4 kB/s | 1.40× | 28% |
| IQ, medium wave | 384 kHz | 1534.9 kB/s | 819.5 kB/s | 1.87× | 47% |
| IQ, quiet band | 384 kHz | 1534.9 kB/s | 997.8 kB/s | 1.54× | 35% |
| IQ, busy band | 384 kHz | 1534.9 kB/s | 1039.0 kB/s | 1.48× | 32% |
| IQ | 12 kHz | 47.9 kB/s | 34.4 kB/s | 1.39× | 28% |
| squelched (all silence) | 12 kHz audio | 24.0 kB/s | 0.4 kB/s | 60× | 98% |

For comparison on the same data: zstd achieves 0.97–0.99× (expansion), and a
fixed first-order predictor with the same Rice coder achieves 1.08–1.56×. The
adaptive cascade is what turns the second of those into the table above.

### 10.2 Cost

Per packet, on one x86-64 desktop core. The figure that matters for a receiver
is not throughput but what fraction of a core one stream costs, since it serves
many at once.

| stream | samples/packet | packets/s | encode | decode | % of a core, each way |
|---|---|---|---|---|---|
| audio 12 kHz | 240 | 50 | 16 µs | 17 µs | 0.09% |
| audio 24 kHz | 480 | 50 | 36 µs | 38 µs | 0.19% |
| IQ 12 kHz | 480 | 50 | 19 µs | 19 µs | 0.09% |
| IQ 384 kHz | 720 | 1099 | 27 µs | 28 µs | 3.0% |

In throughput terms that is 13–15 Msample/s on the real cascade and about
26 Msample/s on the complex profile — the cascade does four passes over the data
where the complex profile does one.

Go and C++ measure within about 15% of each other on the same vectors. Encode
and decode cost
essentially the same, because they run the same filter arithmetic in opposite
directions — a symmetry worth knowing when planning a deployment, since it means
a client pays what the server pays.

For scale: a receiver serving twenty simultaneous 384 kHz IQ sessions spends
about 66% of one core encoding them. The predecessor arrangement — the same
packets through zstd, which could not compress them — cost 0.8% of a core per
session and saved nothing.

Pure Python runs about a hundred times slower than C++, which is enough for
demodulated audio and narrowband IQ in real time and not enough for wideband IQ.
The predictor is strictly sequential and adapts per sample, so it cannot be
vectorised or handed to a library; that is a property of the algorithm, not of
the port.

### 10.3 Rice parameter range

Observed `k` values, which say how much of each codeword is the raw remainder:

| stream | k |
|---|---|
| CW | 0–5 |
| SSB voice | 4–6 |
| SSB data | 5–9 |
| AM | 6–9 |
| NFM | 7–13 |
| IQ 384 kHz medium wave | 5–12 |
| IQ 384 kHz busy band | 8–10 |
| IQ 12 kHz | 8–11 |

---

## 11. What distinguishes this design

Most of the individual pieces here are standard: adaptive linear prediction,
sign-sign LMS, Rice coding, delta-coded metadata. What is distinctive is the
particular combination, and a small number of decisions inside it that are not
the obvious ones.

**1. Exact integer backward adaptation as an interoperability requirement, not
an implementation detail.** Backward-adaptive prediction is normally an internal
matter: one codebase encodes and decodes, and it agrees with itself. Here the
arithmetic is *specified* — the rounding rule, the leak, the sign convention,
the clamp, the stage order — precisely so that independent implementations in
different
languages agree bit for bit. Four implementations in this repository, in Go,
Python, C++ and JavaScript, reproduce the reference vectors sample for sample,
and three of them reproduce the encoder byte for byte. Very few adaptive
lossless codecs make this a specified property.

**2. A tap bound chosen to keep a float64 implementation exact.** The clamp at
2^24 is presented above as overflow headroom, which it is. Its more interesting
consequence is that every product and partial sum stays under 2^53, so a
JavaScript implementation using ordinary numbers is not approximately right but
*exactly* right. That is what makes a browser a first-class decoder for a
lossless format, with no BigInt, no WebAssembly and no 64-bit emulation. The
bound was chosen for this; it is two orders of magnitude above anything
adaptation actually produces.

**3. The predictor is declared, never inferred.** Three otherwise-spare bits
make each packet state which predictor coded it. This costs nothing and moves
predictor selection permanently to the sender, where it can be retuned — per
band, per mode, per measurement — without touching a deployed decoder. §6.1
shows this is not a theoretical benefit: which predictor wins genuinely depends
on the band.

**4. A cascade of short adaptive filters rather than one long one.** For
oversampled demodulated audio, four stages of orders 8/8/4/2 beat a single
order-32 filter by a wide margin (1.711 against 1.362 on the same capture) while
costing less. The usual instinct is to lengthen the filter; the measurement says
to deepen the cascade.

**5. Three body forms, so the format cannot lose.** Coded, verbatim, or absent.
The verbatim escape means the body can never expand, so a saturated front end
costs the header and nothing more. The absent body means digital silence costs
*only* the header — 8 bytes for a block that would otherwise Rice-code to 30
bytes of stop bits, which matters because a squelched receiver produces silence
indefinitely. Neither is exotic on its own; having both, chosen per packet and
signalled in bits the header already had, is what makes the worst case
uninteresting.

**6. A leak that runs on signal and stops on silence.** Sign-sign LMS has no
restoring force, and left alone its taps walk until they hit the bound — on
medium wave, in ninety seconds, turning a compressor into an expander with no
error raised anywhere. The fix is a leak; the interesting part is where it does
*not* run. Adaptation is skipped entirely on a zero error, and the error over
digital silence is zero, so silence preserves whatever the filter had learned
and a receiver coming out of a long squelched period predicts well immediately
instead of re-converging. The taps are held in check by the signal and left
alone by its absence, which is the opposite of what an unconditional decay
would do.

**7. A metadata header that is delta-coded but periodically self-describing.**
7.7 bytes average against 37 fixed, with a defined point every few seconds at
which a reader that arrived late learns the stream parameters. The separation is
deliberate and is stated honestly in §8.3: the header resynchronises, the
predictor does not, and the reason the predictor does not is a measured
trade-off rather than an oversight.

**8. A provably-identity fast path.** The clamp-elision rule of §6.2.6 lets an
implementation skip a bounds check for an entire packet on a proof about the
step size, rather than on a heuristic. Two implementations, one eliding and one
not, produce identical bytes. That is what makes it safe to optimise a format
whose correctness depends on both ends computing the same thing.

**9. It is specified against measurement.** Every parameter in §6 has a
measurement behind it and the alternatives that lost are recorded with their
numbers: forward-adaptive prediction (1.29 against 1.32), arithmetic coding
(level, at far greater cost), continuous `k` adaptation (worse), FLAC-style
partitioning (no change), deeper IQ cascades (1.391 against 1.396 at double the
cost). A reader can tell which parts of the design are load-bearing and which
are conventional.

---

## 12. Design invariants and permitted variation

This section states what the design *is*, as distinct from the particular
numbers this specification happens to fix. It exists because the two are
routinely confused: an implementation that changes a step size or a filter order
is the same design, and one that keeps every number but transmits its
coefficients is not.

### 12.1 Essential characteristics

The design is: **a lossless integer codec for 16-bit radio sample streams in
which each sample is predicted by a backward-adaptive filter whose coefficients
are recomputed identically by both ends and never transmitted; the prediction
residual is entropy-coded with a per-block parameter; the block carries a
verbatim fallback and an omitted-body form for all-zero blocks; and a
delta-coded metadata record transmits only fields that have changed, becoming
periodically self-describing.**

Concretely, all of the following are essential. Remove or invert any one and the
result is a different design, however similar the remaining numbers:

1. **Backward adaptation.** Predictor coefficients are derived from already-coded
   samples by both ends. No coefficient, and no filter state, is ever
   transmitted. (Forward adaptation — fitting coefficients per block and sending
   them — is the alternative, and it measured worse here.)
2. **Exact integer arithmetic with a specified rounding rule.** The two ends
   agree because they compute the same thing, not because they exchange
   anything. Any arithmetic whose result is not specified to the bit breaks the
   format.
3. **Prediction error, not samples, is entropy-coded**, with the coding
   parameter derived from the block's own statistics and carried in the block.
4. **A sign-based, division-free update** whose step is independent of the
   magnitude of the error and of the data — the property that makes it
   integer-exact.
5. **A leak on that update, skipped when the error is zero.** A sign-based
   update has no restoring force, so without a leak the taps walk until they
   reach whatever bound exists; and the skip is what makes the leak stop at the
   edge of silence rather than draining the filter through it. Both halves are
   essential: either alone is a different, and worse, design.
6. **Cascaded stages**, each predicting the residual of the previous, applied in
   reverse on decode.
7. **A distinct filter form for complex baseband** that predicts I and Q
   jointly, rather than as two independent real streams.
8. **Per-block declaration of the predictor configuration**, so that a decoder
   obeys rather than infers, and an unrecognised configuration is a hard failure
   rather than a fallback.
9. **A verbatim fallback selected by comparing the coded size against the raw
   size**, so the body never expands.
10. **An omitted body for all-zero blocks**, with both ends advancing the
    predictor over the implied zeros.
11. **Predictor continuity across every block form** — coded, verbatim and
    omitted alike — so that filter state never diverges.
12. **A metadata record that transmits changed fields only**, with a flags byte
    indicating which are present, and a periodic point at which everything is
    retransmitted.

### 12.2 Parameters that may vary

The following are settings, not design elements. Every table below is measured
on this repository's vectors, so the absolute ratios are lower than §10.1; what
matters here is how little they move. The design's behaviour is
continuous in all of them: the measurements below show that changing them moves
the compression ratio by a few percent, not into a different regime. An
implementation that changes any of them is implementing the same design.

**Filter step size (µ).** The reference values are 16 and 32 in Q16 units. A
factor of two either way costs a few percent:

Audio cascade, every stage's µ scaled together:

| µ, relative to reference | CW | SSB data |
|---|---|---|
| ×0.25 | 4.269 | 1.468 |
| ×0.5 | 5.050 | 1.585 |
| **×1 (16/16/32/32)** | **5.280** | **1.711** |
| ×2 | 5.241 | 1.825 |
| ×4 | 4.963 | 1.850 |

Complex profile, order 16:

| µ | IQ 12 kHz | IQ 384 kHz | IQ medium wave |
|---|---|---|---|
| 4 | 1.391 | 1.559 | 1.476 |
| 8 | 1.420 | 1.583 | 1.524 |
| **16** | **1.447** | **1.598** | **1.589** |
| 32 | 1.464 | 1.605 | 1.653 |
| 64 | 1.476 | 1.606 | 1.663 |

The reference values sit near a broad optimum; the curve is flat around them.
Any value in roughly ×0.5 to ×4 is the same design differently tuned.

**Leak shift.** The reference values are 14 for the complex stage and 16 for the
real cascade (§6.2.3), which put the equilibrium tap magnitude at roughly
`mu·2^s`. The complex measurements in §6.2.3 span shifts 8 to 18, and the worst
of them costs 1.58× the bits of the best, so another value in that range is the
same design at a different time constant. What is *not* a variation is having no leak at
all, or running it on a zero error: see §12.1, items 4 and 5. The leak shift
must move with the tap scale, since it is expressed relative to it.

**Filter order.** The reference complex order is 16. Order 4 to 32 spans about
5%:

| order | IQ 12 kHz | IQ 384 kHz | IQ medium wave |
|---|---|---|---|
| 4 | 1.398 | 1.535 | 1.613 |
| 8 | 1.434 | 1.575 | 1.564 |
| **16** | **1.447** | **1.598** | **1.589** |
| 24 | 1.451 | 1.613 | 1.601 |
| 32 | 1.469 | 1.617 | 1.627 |

**Number and shape of cascade stages.** Any cascade of short stages of
decreasing order behaves the same way; see the table in §6.2.7. Two stages, four
stages or five are the same design.

**Fixed-point scale of the taps.** Q16 is the reference. Q12 and Q20 at the
equivalent real step size and the equivalent leak shift (10 and 18, since a leak
shift is relative to the tap scale) both measure 1.598 against Q16's 1.598 on a
384 kHz capture — the same design at a different resolution. What is essential
is that the scale is fixed, integer, and identical at both ends, and that the
leak shift moves with it.

**The tap bound.** 2^24 is chosen for the float64 property of §11(2). Any bound
large enough never to fire in practice and small enough to prevent overflow is
the same design; only agreement between the two ends is essential.

**History window management.** Linear with a periodic slide, or circular, or any
other arrangement. Nothing about it is transmitted.

**Clamp elision.** Present or absent; provably identical output either way.

**Block size.** Anything from a few dozen to a few thousand samples, and it need
not be constant. Measured across a 32-fold range:

| samples per block | CW | SSB data | IQ 384 kHz |
|---|---|---|---|
| 60 | 5.065 | 1.687 | 1.572 |
| 240 | 5.280 | 1.711 | 1.593 |
| 960 | 5.329 | 1.715 | 1.597 |
| 1920 | 5.360 | 1.713 | 1.597 |

**Entropy coder parameter selection.** Deriving `k` from the mean magnitude is
one method; deriving it from a quantile, from the previous block, or by trying
several and keeping the best are all the same design. The parameter must be
carried in the block.

**Entropy coder family.** Rice is used because it is within about 3% of the
residual entropy (§6.3.4) at negligible cost. Substituting an arithmetic coder,
an rANS coder, or Golomb with a non-power-of-two parameter changes the
compression by a small amount and the design not at all — the design element is
"entropy-code the residual with a per-block parameter", not "use Rice
specifically".

**Resynchronisation interval.** The reference is 5 seconds. Any interval, or
resynchronising only on change, is the same design.

**Metadata field set.** The particular fields — timestamp, rate, channels,
count, two signal-quality readings — are what this application needs. A format
carrying a different set of per-packet metadata through the same
present-if-changed mechanism is the same design.

**Numeric encodings of the metadata.** LEB128 varints, zigzag for signed deltas,
16-bit centidecibels for the quality readings: all replaceable by any equivalent
compact encoding.

**Magic value and flag bit positions.** Fixed for interoperability with this
format, but not design elements. A format with a different magic and a different
bit layout, doing everything in §12.1, is the same design.

**Sample width.** 16-bit is what receivers of this kind deliver. The design
applies unchanged to other integer widths, with the tap bound and accumulator
width adjusted accordingly.

**Channel arrangement.** Interleaved I/Q with a complex predictor is the
reference. Any joint treatment of the two quadrature components is the same
design; treating them as independent real streams is not (§12.1, item 7).

### 12.3 What is not this design

For clarity, these are different designs, not variations:

- Transmitting predictor coefficients, in any form, with any frequency.
- Quantisation anywhere *inside* the coding path: of the residuals, or of the
  samples between the encoder's predictor and the decoder's. That is what breaks
  the two ends' agreement, which is the property everything else here exists to
  protect. Requantising the input ahead of the codec and transmitting the scale,
  as §6.6 does, is not this: the codec still reproduces exactly what it was
  given, and both predictors still run on identical values.
- Entropy-coding the samples themselves rather than a prediction residual.
- An update with no restoring force, or one whose restoring force runs over
  digital silence.
- A predictor whose coefficients are fixed rather than adapting to the stream.
- Requiring floating-point arithmetic whose results are not bit-specified.
- Independent real prediction of I and Q with no joint complex treatment.
- Any arrangement in which a decoder infers the predictor rather than being told
  it, since that is what makes the sender's choice permanent.

---

## 13. Conformance

An implementation conforms if:

1. It decodes every vector in `testdata/` to exactly the samples in the
   corresponding `.pcm` file (§14).
2. It rejects: an unknown or unimplemented profile id; `k > 30`; a shift greater
   than 15; a packet with both escape and silent set; a silent packet carrying a
   body; a non-silent scaled packet with no body; a delta packet arriving before
   any resynchronisation point; a truncated header or bitstream.
3. It runs the predictor across escaped and silent packets.
4. It applies the tap leak of §6.2.3 on every adaptation, with the shift its
   profile's filter form specifies, and skips adaptation entirely — leak
   included — when the prediction error is zero.
5. On a scaled profile it reads the shift byte only when the packet is not
   silent, runs its predictor on the values as coded, and applies the saturating
   left shift of §6.6.2 only on the way out.
6. It treats a codec instance as one stream (§8.1).

Items 4 and 5 are the two that a vector will catch and an inspection will not.
Nothing else in this list can be got wrong quietly: an implementation missing
the leak, or leaking through silence, raises no error, fails no length check and
returns plausible samples that are simply not the right ones, and because the
predictor is backward adaptive it stays wrong for the rest of the stream.

An **encoder** conforms if a conforming decoder reconstructs its input exactly —
where for a scaled profile "its input" means the samples after requantisation.
Byte-identity with the reference encoder is *not* required: an encoder may
choose `k` differently, resynchronise more often, take the escape earlier, or
choose a different shift.

The implementations in this repository nonetheless require byte-identity of
their encoders against the committed vectors, because it is a far sharper test
of a port than round-tripping: a predictor that is subtly wrong in a way its own
decoder reproduces will round-trip perfectly and still be wrong.

---

## 14. Conformance vectors

`testdata/` holds pairs of files: `NAME.pcm` is raw little-endian `int16`
samples, `NAME.uiq` is the encoded stream in the container of §9.

| vector | rate | channels | frames/packet | what it covers |
|---|---|---|---|---|
| `audio-cw-12k` | 12 kHz | 1 | 240 | a narrow tone in a mostly empty channel; the highest ratio the format reaches |
| `audio-usb-12k` | 12 kHz | 1 | 240 | an ordinary voice-bandwidth demodulated channel |
| `iq-12k` | 12 kHz | 2 | 240 | narrowband complex baseband |
| `iq-384k` | 384 kHz | 2 | 360 | wideband complex baseband on a busy band |
| `iq-384k-mw` | 384 kHz | 2 | 360 | strong carriers 60 dB above the noise floor, which produce the longest unary runs in the set |
| `rice-63` | 96 kHz | 2 | 240 | generated: a Rice codeword whose unary run consumes a full 64-bit accumulator (§6.3.3) |
| `edge-cases` | mixed | mixed | mixed | silence, escapes, count and rate changes, a profile change, quality readings that change and then stop |
| `iq-scaled` | 384 kHz | 2 | 360 | generated from the medium-wave capture: profile 2 at five different shifts including zero, a silent scaled packet with no shift byte, an escaped scaled packet, and crossings out to profile 0 and back (§6.6) |

The first five are real receiver traffic. Synthetic signals cannot replace them:
every bug found while developing this format was triggered by a property of real
RF that a generator does not reproduce — unary runs long enough to overflow a
bit writer, band statistics that decide which predictor wins, and residual
distributions that determine whether the entropy coder is well matched. The last
three are generated, because a live capture happens to contain none of the
conditions that are easiest to get wrong.

None of them is long enough to demonstrate what the leak of §6.2.3 is *for* —
that takes minutes of one particular kind of signal — but every one of them is
long enough to detect whether it is present, because it changes the coded bytes
from the first packets. That is the division of labour between the vectors and
the specification: the vectors say whether an implementation matches, and §6.2.3
says why the constants are what they are.

---

## Appendix A: a packet, byte by byte

The first two packets of `testdata/audio-cw-12k.uiq`. Container length prefixes
are omitted.

### First packet — a resynchronisation point, 22-byte header

```
50 43 4d 34   magic 0x344D4350
69            flags = 0110 1001
              bit 6 quality, bit 5 metadata, bit 3 count, profile 1
00 00 2a 36 fe 9c 97 17
              timestamp, uint64 LE = 1700000000000000000
f0 01         sample_count, uvarint = 240
e0 5d         sample_rate, uvarint = 12000
01            channels = 1
00 80         baseband_power, int16 LE = -32768  (no reading)
00 80         noise, int16 LE = -32768           (no reading)
--- body, 127 bytes ---
02            Rice parameter k = 2
cf 8f 8f ...  bitstream, LSB-first
```

240 samples, 480 bytes raw, in a 149-byte packet: 3.22×.

### Second packet — steady state, 9-byte header

```
50 43 4d 34   magic
01            flags = 0000 0001
              no metadata, no quality, no count: profile 1 only
80 b4 89 13   timestamp delta, zigzag varint = +20000000 ns (20 ms)
--- body, 115 bytes ---
02            k = 2
af 6b ef ...  bitstream
```

Nothing but the timestamp changed, so nothing but the timestamp was sent. The
rate, channel count, sample count and both quality readings carry forward from
the packet before.

Reading the varint: `80 b4 89 13` → groups `0000000`, `0110100`, `0001001`,
`0010011` (least significant first) → 40,000,000 → zigzag-decoded → +20,000,000.

Over the whole 150-packet vector the mean header is 9.09 bytes, against 37 for a
fixed record.

---

## Appendix B: sibling frames without a payload codec

The deployment this format comes from sends lossy frames of another kind on the
same socket, selected per packet, and those frames reuse this header machinery
in a reduced form. An implementation reading such a socket needs to recognise
them; an implementation reading only UberSDR-IQ streams can ignore this
appendix.

Such a frame carries **no magic** and a two-bit flags byte:

| bit | mask | meaning |
|---|---|---|
| 0 | `0x01` | quality: the pair follows |
| 1 | `0x02` | metadata: full timestamp, sample rate and channel count follow |

then the same timestamp (full or delta), sample rate, channel count and quality
fields, in the same encodings, followed by the lossy payload. There is no sample
count, because that payload states its own length; and no escape, silent or
profile bits, because there is no predictor.

The absence of a magic is safe rather than merely convenient. Frames are told
apart by elimination — an UberSDR-IQ magic, else the other kind — and the two
cannot collide: the magic's first byte is `0x50`, which has bit 4 set, while
such a frame's first byte is a flags byte using only bits 0 and 1 and so never
exceeds `0x03`. A shared magic would only have made confusion improbable; this
makes it impossible.

---

## Appendix C: complete decoder pseudocode

Everything a decoder needs, in the order a reader would build it. All values are
integers; every intermediate stays within a signed 64-bit range, and within
2^53, so a language with only float64 integers is exact here (§6.2.6). `>>` below
is an arithmetic shift on a non-negative value only — the two places where a
negative would be shifted, `roundShift` and `leak`, are written out.

### C.1 Arithmetic helpers

```
sgn(v)            = -1 if v < 0, 0 if v == 0, +1 if v > 0

roundShift(v, s)  = round to nearest, ties away from zero, of v / 2^s
                    if v < 0:  -(((-v) + 2^(s-1)) >> s)
                    else:       (( v  + 2^(s-1)) >> s)

leak(w, s)        = magnitude of w over 2^s, truncated towards zero
                    if w < 0:  -((-w) >> s)
                    else:       ( w  >> s)

clamp(w)          = min(2^24, max(-2^24, w))

clamp16(v)        = min(32767, max(-32768, v))

zigzag_undo(u)    = (u >> 1) ^ -(u & 1)      # u unsigned 32-bit, result signed
```

### C.2 One stage

A stage is `order`, `mu`, `leak_shift` (14 complex, 16 real), taps and history.
Taps start at zero; history starts as `order` zeros. `h[0]` is the oldest of the
`order` history entries and is the one tap `w[0]` weighs.

```
real stage:
    w[0..order-1] = 0
    h[0..order-1] = 0

    predict():
        return roundShift( sum over j of w[j] * h[j], 16 )

    adapt(e):
        if e == 0: return                      # REQUIRED -- see 6.2.4
        m = mu * sgn(e)
        for j in 0..order-1:
            w[j] = clamp( w[j] + m * sgn(h[j]) - leak(w[j], 16) )

    push(x):
        drop h[0]; append x                    # h stays order deep, oldest first

    inverse(e):                                # decoder direction
        p = predict();  x = e + p;  adapt(e);  push(x);  return x

    forward(x):                                # encoder / advance direction
        p = predict();  e = x - p;  adapt(e);  push(x);  return e

complex stage:
    wr[0..order-1] = wi[0..order-1] = 0
    hr[0..order-1] = hi[0..order-1] = 0

    predict():
        pr = sum over j of  wr[j]*hr[j] - wi[j]*hi[j]
        pi = sum over j of  wr[j]*hi[j] + wi[j]*hr[j]
        return roundShift(pr, 16), roundShift(pi, 16)

    adapt(er, ei):
        if er == 0 and ei == 0: return         # REQUIRED -- see 6.2.5
        mr = mu * sgn(er);  mi = mu * sgn(ei)
        for j in 0..order-1:
            hrs =  sgn(hr[j])
            his = -sgn(hi[j])                  # conjugate
            wr[j] = clamp( wr[j] + mr*hrs - mi*his - leak(wr[j], 14) )
            wi[j] = clamp( wi[j] + mr*his + mi*hrs - leak(wi[j], 14) )

    push(xr, xi):
        drop hr[0], hi[0]; append xr, xi

    inverse(er, ei):
        pr, pi = predict()
        xr, xi = er + pr, ei + pi
        adapt(er, ei);  push(xr, xi);  return xr, xi

    forward(xr, xi):
        pr, pi = predict()
        er, ei = xr - pr, xi - pi
        adapt(er, ei);  push(xr, xi);  return er, ei
```

`adapt` reads the same history window `predict` did; `push` happens after both.
`leak` is taken from the tap's value before this update, and the clamp is
applied to the whole expression. In `adapt`, `w[j]` and `wi[j]` each leak from
themselves.

### C.3 The codec

```
new_codec(profile):
    0: one complex stage,  order 16, mu 16                      # iq-complex-o16
    1: four real stages,   orders 8, 8, 4, 2                    # audio-real-8/8/4/2
                           mus    16, 16, 32, 32
    2: one complex stage,  order 16, mu 16, scaled              # iq-complex-o16-scaled
    anything else: FAIL. Never fall back.

    step = 2 if the stages are complex else 1
    scaled = true only for profile 2

decode_samples(codec, residuals, count):        # cascade in REVERSE order
    for i in 0, step, 2*step, ... up to count:
        a = residuals[i];  b = residuals[i+1] if step == 2
        for stage in reverse(codec.stages):
            a, b = stage.inverse(a, b)          # b absent for a real stage
        out[i] = a;  out[i+1] = b if step == 2
    return out

advance(codec, samples, count):                 # cascade in FORWARD order
    for i in 0, step, 2*step, ... up to count:
        a = samples[i];  b = samples[i+1] if step == 2
        for stage in codec.stages:              # first to last
            a, b = stage.forward(a, b)
        # residuals discarded
```

`advance` is used for escaped and silent packets. For a silent packet the
samples are `count` zeros; because prediction over an all-zero history is zero
the error is zero, `adapt` returns immediately, and the taps are left exactly as
they were.

### C.4 The Rice bitstream

```
rice_decode(body, count):
    require len(body) >= 1
    k = body[0];  require k <= 30
    bits = a reader over body[1..], taking the LEAST significant bit of each
           byte first, and bytes in increasing address order
    for i in 0..count-1:
        q = 0
        while next_bit() == 1:  q += 1          # unary; unbounded, see 6.3.3
        rem = 0
        for b in 0..k-1:  rem |= next_bit() << b
        u = (q << k) | rem
        r[i] = zigzag_undo(u)
    return r
```

`next_bit` past the end of `body` is a truncated bitstream and MUST be an error.
An implementation buffering bits in a 64-bit accumulator must treat a shift of
the full accumulator width as producing zero (§6.3.3).

### C.5 The packet

```
state per stream:
    header:  have_metadata, last_ts, rate, channels, count, power, noise
    codec:   profile, stages[]   (taps and history, all zero initially)

decode(packet):
    require len(packet) >= 5 and le_u32(packet[0..4]) == 0x344D4350
    flags = packet[4];  off = 5
    profile = flags & 7
    escape  = flags & 0x80;  silent = flags & 0x10
    require not (escape and silent)

    if flags & 0x20:                       # metadata: a resync point
        last_ts = le_u64(packet[off..off+8]);  off += 8
    else:
        require have_metadata
        delta, off = varint(packet, off);  last_ts += delta

    if flags & 0x08:  count, off = uvarint(packet, off)
    if flags & 0x20:
        rate, off = uvarint(packet, off)
        channels = packet[off];  off += 1
        have_metadata = true
    else:
        require have_metadata
    if flags & 0x40:
        power = le_i16(packet, off);  noise = le_i16(packet, off+2);  off += 4
    require rate > 0 and channels > 0 and count > 0
    # every read above must also check it stays inside the packet

    if profile != codec.profile:  codec = new_codec(profile)   # fails if unknown
    step = 2 if codec.is_complex else 1
    require count % step == 0

    if silent:
        require off == len(packet)         # no body, and no shift byte either
        advance(codec, count zeros, count)
        return count zeros

    shift = 0
    if codec.scaled:                       # profile 2 only, and only if not silent
        require off < len(packet)
        shift = packet[off];  off += 1
        require shift <= 15

    if escape:
        require len(packet) - off >= 2*count
        samples = le_i16 array of count from packet[off..]
        advance(codec, samples, count)     # forward direction, residuals discarded
    else:
        residuals = rice_decode(packet[off..], count)
        samples = decode_samples(codec, residuals, count)

    if shift != 0:                         # only on the way out -- see 6.6.2
        for i in 0..count-1:  samples[i] = clamp16(samples[i] << shift)
    return samples
```
