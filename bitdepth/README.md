# An experimental reduced-bit mode

Does UberSDR-IQ need all sixteen bits? This is a viability test: it asks whether
a lossy reduced-depth mode would cost anything a user notices, using FT8 decodes
as the judge because they are an objective pass/fail on weak-signal content
rather than an opinion about audio.

**Answer: 10 bits loses nothing.** All 96 decodes come back, none are gained,
none degrade to needing a priori information, and no decode's reported SNR moves
by more than 1 dB — for 32% fewer bytes than the lossless mode.

**And a 10-bit ceiling with the noise reading used underneath it is better than
either.** A fixed 10-bit mode costs 491,870 bytes; the same ceiling with the
receiver's 100 ms noise reading deciding how much of that budget each packet
actually needs costs 444,193 for exactly the same 96 decodes — 10% less — and
380,680 if pushed, still with every decode intact. The header already carries
the signal-quality pair (SPEC.md §5.6) this needs.

## What the mode would be

A reduced-depth mode is a front end to the format, not a change to it.
Requantise each packet, hand the small values to the existing predictor and Rice
coder, and put the scale in the header so the receiving end can undo it. The
body codec is untouched; the saving comes entirely from the numbers going into
it being smaller, which shortens every Rice codeword.

The scale is a power of two, so applying and undoing it are shifts. That keeps
the path in integers, which the format requires — both ends must arrive at
bit-identical filter state (SPEC.md §6.2), and a floating-point gain would not
guarantee that.

Two ways of choosing that scale are tested:

- **`bfpN` — peak-driven.** Each packet gets the smallest shift that fits its
  own peak into N bits, so a quiet packet keeps more resolution than a loud one.
  One shift per packet is what the header would carry: a nibble against a packet
  of hundreds of bytes.
- **`snrM` / `snrfastM` — noise-driven.** The depth is not fixed at all. The
  shift is chosen to hold the quantisation floor M dB below the measured noise
  floor, and whatever depth that implies is what the packet gets. Readings
  arrive every 100 ms, as the receiver supplies them; `snrM` smooths each over a
  second, `snrfastM` computes each from only the last 100 ms. See below.

`trunc10` is a control on whether per-packet scaling earns its complexity: one
fixed scale for the whole stream. It is **not a candidate** — it picks its scale
from the whole file's peak, so it knows the loudest sample before emitting the
first one. It bounds what a fixed scale could do rather than competing.

## What is measured

`../testdata/60s-20m-ft8.wav`: 60 s of 20 m FT8, 12 kHz mono int16, 3 kHz
passband, four complete 15 s periods. Trimmed to 721,440 samples so it holds a
whole number of 240-sample packets, which the lossless control arm needs in
order to round trip exactly.

Every arm goes through the real codec — `uiq encode`, `uiq decode` — and the run
asserts the round trip before scoring anything. The control arm must come back
bit-identical to the input; each reduced-depth arm must return the requantised
values it was given. Then `jt9 -8` decodes all four periods of each arm and
`compare.py` matches decodes by (period, message).

Two things are scored, because a decode count alone is a weak test:

- **Which messages came back.** Matched on message text, not the SNR and dt
  columns. A trailing `a1` marks a decode that needed a priori information;
  those are counted separately, because a message that only returns with AP has
  degraded even though it still appears.
- **How strongly they came back.** The per-decode SNR difference against the
  lossless arm. A decode scraping through 3 dB weaker has lost margin even
  though it still counts, and only this catches that.

These are independent. An arm can return every message and still have shifted
their reported strengths, which is what the `SNR unchanged` column below tracks:
it counts, **out of the decodes that did come back**, how many jt9 reported at
exactly the same SNR. It is not a count of successful decodes — `lost` is. jt9
reports SNR in whole decibels, so a decode outside that count is one bin away,
not meaningfully weaker.

## Running it

    ./run.sh

Needs `jt9` (wsjt-x), Go, numpy.

## Results

Requantisation, before the codec sees it:

| mode | SNR | max err | shifts | mean depth |
|---|---|---|---|---|
| bfp8 | 40.76 dB | 64 | 0-7 | 8 fixed |
| bfp9 | 46.77 dB | 32 | 0-6 | 9 fixed |
| **bfp10** | **52.79 dB** | **16** | **0-5** | **10 fixed** |
| bfp12 | 64.61 dB | 4 | 0-3 | 12 fixed |
| snr6 | 41.29 dB | 256 | 0-9 | 8.03 bits |
| snr10 | 45.47 dB | 128 | 0-8 | 8.72 bits |
| snr14 | 48.95 dB | 64 | 0-7 | 9.25 bits |
| snr18 | 53.28 dB | 63 | 0-7 | 9.94 bits |
| trunc10 | 50.57 dB | 16 | 5 fixed | 10 fixed |

Wire size through the codec, and what each arm cost. `lost` is how many messages
failed to decode; `SNR unchanged` counts how many of the decodes that *did* come
back were reported at exactly the lossless arm's SNR — a margin measure, not a
success rate:

| mode | bytes | ratio | kB/s | decodes | lost | AP-only | mean dSNR | SNR unchanged |
|---|---|---|---|---|---|---|---|---|
| pcm16 (lossless) | 724,996 | 1.99x | 12.06 | 96 | - | - | - | 96 / 96 |
| bfp12 | 623,273 | 2.32x | 10.37 | 96 | 0 | 0 | +0.00 dB | 88 / 96 |
| **bfp10** | **491,870** | **2.93x** | **8.18** | **96** | **0** | **0** | **+0.04 dB** | **84 / 96** |
| snr18 | 477,451 | 3.02x | 7.94 | 96 | 0 | 0 | +0.04 dB | 82 / 96 |
| snrfast14 | 447,034 | 3.23x | 7.44 | 96 | 0 | 0 | +0.02 dB | 72 / 96 |
| snr14 | 444,242 | 3.25x | 7.39 | 96 | 0 | 0 | +0.00 dB | 70 / 96 |
| bfp9 | 432,361 | 3.34x | 7.19 | 96 | 0 | 0 | -0.05 dB | 65 / 96 |
| trunc10 (offline bound) | 426,864 | 3.38x | 7.10 | 96 | 0 | 0 | -0.03 dB | 75 / 96 |
| snr10 | 416,762 | 3.46x | 6.93 | 96 | 0 | 0 | -0.02 dB | 69 / 96 |
| **snrfast6** | **382,540** | **3.77x** | **6.36** | **96** | **0** | **0** | **-0.14 dB** | **55 / 96** |
| snr6 | 381,276 | 3.78x | 6.34 | 95 | 1 | 0 | -0.16 dB | 51 / 95 |
| bfp8 | 380,074 | 3.80x | 6.32 | 94 | 2 | 1 | -0.20 dB | 49 / 94 |

Every arm round-tripped through the codec exactly, and the lossless arm returned
the input bit for bit.

What the reduced arms lose:

    everything except snr6 and bfp8   nothing
    snr6    lost   period 2   -9 dB    CQ SQ8AA KN19
    bfp8    lost   period 2  -24 dB    <...> AB1HL FN42
            lost   period 2   -9 dB    CQ SQ8AA KN19
            AP     period 1  -18 dB    CQ KA1MXL FN41

## Using the signal quality the header already carries

SPEC.md §5.6 puts two readings in the header: baseband power and a noise figure,
in signed centidecibels, sent when they change and carried forward otherwise,
and present on every resynchronisation point. Today they are passengers —
"nothing in the codec reads them".

They describe precisely the quantity a depth decision needs. The span a packet
must represent runs from its peak, which must not clip, down to below the band's
own noise floor, which must not be raised. Peak-to-noise is what those two
numbers are.

So instead of fixing the depth and letting the quantisation floor land where it
may, fix the floor — M dB below the measured noise — and let the depth fall out
of it. A noisy band then costs fewer bits and a quiet one more, which is the
right way round and is what a fixed depth cannot do. `snrM` implements that,
recomputing the readings from the samples so the experiment can run without a
live receiver; a real encoder reads them from the receiver it already queries.

**The reading arrives every 100 ms.** That is the cadence the receiver supplies
it at, five packets at 12 kHz, and it matters: `snrM` uses a reading refreshed
every 100 ms but smoothed over a second, `snrfastM` one computed from only the
last 100 ms. The fast one tracks the band's short-term level, and at the
aggressive end that is worth a decode.

Head to head at matched bytes — all three within 0.6% of each other:

| arm | bytes | decodes | lost | AP-only | SNR unchanged |
|---|---|---|---|---|---|
| bfp8 — peak-driven | 380,074 | 94 | 2 | 1 | 49 / 94 |
| snr6 — noise, 1 s smoothing | 381,276 (+0.3%) | 95 | 1 | 0 | 51 / 95 |
| **snrfast6 — noise, 100 ms** | **382,540 (+0.6%)** | **96** | **0** | **0** | **55 / 96** |

**At the same size, driving the depth from the noise reading loses nothing where
driving it from the peak loses two.** Its mean depth is 7.99 bits. The decodes
`bfp8` drops are `<...> AB1HL FN42` at −24 dB, the weakest in the file and at
FT8's published decode floor, and `CQ SQ8AA KN19` at −9 dB; `snrfast6` keeps
both, and needs no AP where `bfp8` did.

That is the case a peak-driven rule is structurally blind to. `bfp8` puts a big
shift on a loud packet because the peak says so; the noise floor, which is what
a faint signal is actually competing against, never enters the decision. Note
that `snrfast6` reaches this with a *coarser* worst case — a max error of 256
against `bfp8`'s 64. It is not being more careful overall, it is being careful in
the right places.

The advantage closes as the budget grows, because there is nothing left to
recover:

| arm | bytes | lost | SNR unchanged |
|---|---|---|---|
| bfp9 | 432,361 | 0 | 65 / 96 |
| snr14 | 444,242 (+2.7%) | 0 | 70 / 96 |
| **bfp10** | **491,870** | **0** | **84 / 96** |
| snr18 | 477,451 (−2.9%) | 0 | 82 / 96 |

Every arm there returns all 96 messages, so nothing separates them on decodes.
What separates them is margin, and above about 9 bits the two rules converge —
`snr18` reaches 82 unchanged against `bfp10`'s 84 for 3% fewer bytes. Once every
decode already survives, the noise-driven rule has nowhere left to show a gain.

### A ceiling, with the noise reading underneath it

The two ideas combine better than either alone. Take whichever shift is coarser:

    s = max(s_cap, s_noise)

`s_cap` is the shift that fits the packet's peak into the ceiling, so the result
can never need more than the ceiling's worth of bits. `s_noise` is the shift
that holds the quantisation floor a set margin under the measured noise; when
the band is noisy that is the coarser of the two and the packet costs less than
the ceiling would have.

The direction of that `max` is the point: **the noise reading can only ever save
bytes, never spend them.** Bandwidth stays bounded by the ceiling, so a client
can still budget for the stream, while the reading decides how much of the
budget is actually needed. A pure noise-driven rule has no such ceiling — its
depth ranged 2 to 14 bits on this file — and an encoder that can demand more
bits when the band goes quiet is hard to provision for.

Measured, with a 10-bit ceiling and the margin as the only knob:

| arm | bytes | kB/s | vs bfp10 | decodes | lost | SNR unchanged | mean depth |
|---|---|---|---|---|---|---|---|
| bfp10 — ceiling only, peak-driven | 491,870 | 8.18 | — | 96 | 0 | 84 / 96 | 10.00 |
| cap10m18 | 473,133 | 7.87 | −3.8% | 95 | 1 | 69 / 96 | 9.36 |
| **cap10m14** | **444,193** | **7.39** | **−9.7%** | **96** | **0** | **72 / 96** | **9.08** |
| cap10m10 | 414,996 | 6.90 | −15.6% | 96 | 0 | 62 / 96 | 8.61 |
| **cap10m6** | **380,680** | **6.33** | **−22.6%** | **96** | **0** | **55 / 96** | **7.97** |
| bfp8 — fixed 8-bit, for scale | 380,074 | 6.32 | −22.7% | 94 | **2** | 49 / 94 | 8.00 |

Every capped arm is cheaper than the plain 10-bit ceiling, as the `max` requires.
`cap10m14` returns every decode for 10% fewer bytes than `bfp10`. `cap10m6`
returns every decode at **the same size as a fixed 8-bit mode that loses two** —
6.33 kB/s against 6.32 — which is the whole argument for using the reading,
restated at a bounded bitrate.

The margin is the only tuning knob, and it is a pure sender-side one: the packet
carries a shift, not a policy, so an encoder can ship at 14 dB and move to 10 dB
later without any client changing. That is what makes committing to a single
lossy mode safe — the mode is "10-bit ceiling", and how well it uses that
ceiling can improve for the life of the deployment.

Two caveats on the table. `cap10m18` losing a decode that `cap10m14` keeps, with
slightly worse SNR agreement despite quantising more finely, is not a trend —
it is one decode sitting on the boundary, and it marks the resolution of this
experiment rather than a property of the margin. And the SNR-unchanged column
falls steadily as the margin tightens, from 84 at the plain ceiling to 55 at
6 dB: those decodes all still arrive, but with less headroom than the lossless
arm gave them. Where to sit on that curve is a judgement about how much margin
is worth how many bytes, and this file cannot settle it alone.

### Can the client be left out of it entirely?

Only partly, and the measurement says where the line falls.

**The policy, yes.** If the packet declares its shift, the encoder chooses the
depth however it likes — per packet, from whatever noise data it holds — and the
decoder applies what arrives without knowing the rule. This is the arrangement
SPEC.md §6.1 already uses for predictor profiles: "predictor selection stays
entirely a sender policy question... A sender may change how it chooses — per
band, per mode, per measurement — and every deployed decoder keeps working." A
receiver could switch from peak-driven to noise-driven, or retune its margin,
years after clients shipped.

**The mechanism, no — or rather, yes but pointlessly.** The tempting version is
to requantise and then shift back before coding, so the client receives an
ordinary int16 stream. That genuinely works: the stream is a perfectly valid
UberSDR-IQ stream, an unmodified decoder returns it bit-exactly, and the encoder
can change depth every packet with nothing on the wire to say so. No header
field, no negotiation, no client change, no version bump.

It just costs bytes instead of saving them:

| depth | bytes | ratio | vs lossless | unmodified decoder |
|---|---|---|---|---|
| 16 (none) | 724,996 | 1.99x | — | exact original |
| 12 | 769,577 | 1.87x | +6.1% | bit-exact |
| 10 | 832,788 | 1.73x | +14.9% | bit-exact |
| 9 | 867,794 | 1.66x | +19.7% | bit-exact |
| 8 | 907,578 | 1.59x | +25.2% | bit-exact |
| 7 | 945,961 | 1.53x | +30.5% | bit-exact |
| 6 | 988,502 | 1.46x | +36.3% | bit-exact |

Every one of those is larger than simply sending all sixteen bits losslessly,
and it gets monotonically worse the harder you quantise: six bits costs 36% more
than never having quantised at all, having also thrown ten bits away. The
transparency and the saving are mutually exclusive, and this is the price of
choosing transparency.

The reason is in the Rice coder. Shifting back multiplies every residual by
2**s, so the parameter `k` rises by `s` and each codeword grows by `s` bits; the
low bits are zero but they are still spent, because Rice codes magnitude and has
no way to say "and eight zeros". On top of that the quantisation noise is not
predictable, so the predictor leaves *larger* residuals than it did on the clean
signal — which is why the curve keeps climbing rather than flattening.

So the client has to be told the shift. It is a nibble in the header, and that
is the whole of what it must learn.

**What it would cost to adopt.** Three things, none free:

- It makes the codec read fields the specification explicitly calls passengers.
  That is a coupling the format does not currently have.
- A stream whose readings are the "no reading" codepoint has no scale, so a
  fallback — peak-driven — is needed anyway.
- Against that: because both ends already share these fields, a derived scale
  need not be transmitted at all. Both compute the same shift from the same
  numbers, which is the same move the backward-adaptive predictor makes with its
  taps. That is a genuinely attractive property, and it is what makes the idea
  worth more than the one decode it recovered here.

## Reading it

**10 bits is viable, and it is not marginal.** Nothing lost, nothing degraded to
AP, no decode moved by more than 1 dB — which is the resolution jt9 reports SNR
in, so 88% are unchanged and the rest sit in an adjacent bin. The decode at the
floor of the mode came through. It saves 32%: 8.18 kB/s against 12.06.

**The decode count alone would have picked 9 bits, and it would have been
wrong.** Both 9 and 10 return all 96 decodes, so on that measure 9 bits looks
free and 12% cheaper. The SNR column separates them. All three return every
message; what differs is how many come back at an unshifted strength — 65 of 96
at 9 bits, 84 at 10 bits, and 88 at 12 bits, which is six bits of headroom over
the quantiser. Ten is where the curve flattens against
what more bits can buy; nine is already spending margin that has not yet shown
up as a lost decode. On a quieter band, or a deeper fade, that spent margin is
what a decode would have needed.

**8 bits is where peak-driven stops being free**: two decodes go, one more needs
AP, and half the survivors come back at a different strength. Noise-driven at the
same size loses none of them. If a reduced-depth mode is built, that is the
argument for driving it from the readings already on the wire rather than from
the packet peak — the gain is concentrated exactly where a fixed depth starts to
hurt.

**Bit depth is not where this format's compression comes from.** Discarding six
of every sixteen bits moved the ratio from 1.99x to 2.93x, not to the 3.2x a
naive bits-in-bits-out account would predict. The predictor was already removing
most of what those low bits carried; what shrinks is the residual, not the
sample.

## What this does not establish

One file, one band, one mode, one 60 s window. FT8 is also a forgiving judge: a
coded mode with 50 Hz occupancy and heavy processing gain. A mode with less
coding gain, a wider signal, or a band with a 60 dB carrier in the passband —
the medium-wave case in `../bench-sdrpp` — would put the quantisation floor
under more pressure than a busy but level 20 m band does.

The noise-driven arms carry an extra caveat: their readings are recomputed from
the samples here rather than taken from a receiver. The 100 ms update cadence is
the real one, but a receiver derives its noise figure through a different
measurement path, and its numbers would not match these exactly. What the
experiment shows is that a reading of this kind, at that cadence, carries enough
information to place the quantiser well; it does not show that a particular
receiver's figures will.

---

This file is licensed under CC BY-SA 4.0 (`../LICENSE-DOCS`).
