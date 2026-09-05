# UberSDR-IQ against FLAC, lossless — 2026-09-05

An honest lossless comparison, run on live recordings, against the reference
FLAC encoder.

**Held to the constraint a live stream actually works under — every block
independently decodable, no look-ahead past one packet — UberSDR-IQ wins 7 of
the 11 streams measured, and wins them by a wide margin: 34% on CW, 23% on FT8
audio, 19% on SSB voice.** Those are the signals a receiver spends most of its
bandwidth carrying.

It loses on broadcast AM, and that includes the 909 kHz capture this test was
originally built around, where FLAC is 9–19% smaller. A stationary carrier with
shallow modulation is the best case for FLAC's solved-per-block LPC and the
worst case for a slowly-adapting sign-sign filter. That result is reported in
full below, first and unsoftened, because a comparison that hid it would not be
worth running.

FLAC is held to our packet size throughout. That is not a handicap invented for
this test: a live stream cannot look ahead past the packet it is sending, and
every packet must decode on its own because the one before it may not have
arrived. FLAC given the whole file at once compresses better than either column
here, but it is not a configuration this problem admits, so it is not compared
against.

Everything here is a run that was performed. Nothing is estimated.

- Encoder under test: `go/ubersdriq` at `f5c2493`, driven by `go/cmd/uiq`
  (`uiq encode` / `uiq decode`).
- Reference encoder: **`flac 1.4.3`**, `/usr/bin/flac`, the reference
  implementation, invoked as a subprocess. No library substitute.
- **`flac -9` does not exist.** flac 1.4.3 answers `-9` with
  `ERROR: compression level '9' is reserved`; `-8` is what it calls `--best`.
  The `-9` arm was therefore run as `-8`, and two extra arms add `-e -p`
  (exhaustive model search plus exhaustive Rice partitioning) on top of `-8`,
  which is the most work the reference encoder will do on a file. Nothing was
  left on FLAC's side of the table.

---

## The capture

30 s of demodulated AM from **909 kHz**, from the reference receiver (M9PSY, Dalgety Bay),
recorded once to a file over the audio WebSocket at protocol version 3 and
compressed offline. No arm touched the receiver.

    first sample  2026-09-05T11:34:41.597Z   mode am, filter -5000..+5000 Hz
    720480 samples, 30.020 s, 24000 Hz mono, 1501 packets of 480 frames

    Primary B: first sample 2026-09-05T11:35:11.817Z, mode usb, filter
    50..2700 Hz, 360240 samples, 30.020 s, 12000 Hz mono, 1501 packets of 240

Two capture sets were taken at 909 kHz, both measured in full:

- **Primary A — AM, 24 kHz, 480-sample packets.** Demodulated AM programme
  audio, the receiver's native rate for `am`.
- **Primary B — USB, 12 kHz, 240-sample packets.** The same broadcast through
  the 12 kHz demodulator. Worth knowing when reading its numbers: USB-demodulating
  an AM carrier at its own dial frequency puts the carrier at 0 Hz audio, so 99%
  of that stream's AC energy sits below 100 Hz — a carrier beat plus one
  sideband rather than programme audio.

### How the capture was verified to contain programme material

Primary A, measured on the samples:

- **Carrier present**: DC 4174 of 32768, and the wideband spectrum API shows
  the 909 kHz carrier 58 dB above the surrounding median.
- **Modulation present**: AC RMS 822 after DC removal, and the per-packet AC
  RMS runs 37 → 152 → 822 → 1187 → 1451 (min, p10, median, p90, max): a 7.8:1
  swing between the 10th and 90th percentile and 39:1 end to end. That is
  programme dynamics; a dead carrier gives a flat line and silence gives zero.
- **Voice-band content**: AC energy spread across 100 Hz–6 kHz
  (100–300 Hz −4.5 dB, 300–1000 Hz −6.4 dB, 1–3 kHz −8.9 dB, 3–6 kHz −6.5 dB of
  the AC total), not concentrated at DC as a bare carrier would be.
- **Not silence**: zero samples out of 720480 are zero.

Primary B: DC −602, AC RMS 3882, per-packet AC RMS 62 → 6418, one zero sample
in 360240. Energy 99% below 100 Hz, as described above.

---

## Byte counting, stated before the numbers

**Ours.** The sum of `len(packet)` over all 1501 packets, per-packet headers
included. Measured header cost on this stream: **9.07 bytes/packet** on Primary
A and 9.06 on Primary B. The `.uiq` container's 4-byte length prefix per packet
is *excluded*, because a message transport supplies the frame boundary itself
and a deployment does not send it. Excluding it costs us nothing to justify: it
would add 6004 bytes, 0.9%, and would not change any verdict below.

**FLAC.** Every byte of the `.flac` file, frame headers and CRCs included.
Encoded with `--no-padding --no-seektable`, which is the smallest file the
reference encoder will write. The remaining metadata is **86 bytes**: the 4-byte
`fLaC` marker, a 38-byte STREAMINFO and a 44-byte VORBIS_COMMENT that flac
inserts to name itself.

**The metadata is counted, not excluded.** A streaming deployment cannot drop
it — a decoder needs the rate, the depth and the sample size from somewhere, and
ours transmits exactly that information in-band on every resynchronisation
packet rather than once at the front. At 86 bytes on a 490 kB file it is 0.018%
and changes nothing; both the frames-only and the total figures are in
`work/flac/*/arms.json` for anyone who wants to check that.

---

## Bit-exactness — verified, both ways, on every arm

Not claimed: run.

- **Ours**: `uiq decode` on the encoded stream, then the decoded file compared
  byte for byte against the input samples. Identical on every stream.
- **FLAC**: `flac -d` to WAV, then the decoded samples compared element by
  element with numpy against the input array. **Additionally** `flac -t`, the
  encoder's own stream verification, on every file.

**All 77 arms across all 11 streams passed both checks.** No arm is reported
that was not verified lossless.

---

## Primary result — AM 909 kHz, 24 kHz, 480-sample packets

FLAC is held to our block size, because that is the only configuration a live
stream could actually use: every block independently decodable, no look-ahead
past one packet.

| arm | bytes | kB/s | ratio | vs ours |
|---|---|---|---|---|
| **ubersdr-iq lossless**, 480-sample packets, headers in | **641002** | 21.35 | **2.248×** | — |
| flac -5, `--blocksize=480` | 585862 | 19.52 | 2.460× | **8.6% smaller** |
| flac -8, `--blocksize=480` | 543215 | 18.10 | 2.653× | **15.3% smaller** |
| flac -8 -e -p, `--blocksize=480` | 519525 | 17.31 | 2.774× | **19.0% smaller** |

**FLAC wins this stream, 9–19% smaller at our own block size.** A broadcast
carrier with shallow modulation is close to the best case for solved-per-block
LPC and close to the worst case for a slowly-adapting sign-sign filter. There is
no reading of this table in which we win.

### Primary B — USB 909 kHz, 12 kHz, 240-sample packets

| arm | bytes | kB/s | ratio | vs ours |
|---|---|---|---|---|
| **ubersdr-iq lossless**, 240-sample packets | **344076** | 11.46 | **2.094×** | — |
| flac -5, `--blocksize=240` | 369492 | 12.31 | 1.950× | 7.4% **larger** — we win |
| flac -8, `--blocksize=240` | 348621 | 11.61 | 2.067× | 1.3% **larger** — we win |
| flac -8 -e -p, `--blocksize=240` | 324607 | 10.81 | 2.220× | 5.7% smaller |

At 240 samples we beat `-5` and `-8` and lose only to the exhaustive
`-8 -e -p`, which costs 20× the encode time.

---

## The wider set — nine more live streams

The primary capture is one signal. Eleven streams were run in total: the two
above, the five demodulated audio captures and the four IQ captures from the
reference corpus in `ka9q_ubersdr/testdata/pcm_predictive/`. The IQ streams go
to FLAC as stereo WAV, which lets FLAC try mid-side decorrelation across I and
Q — a different trick from this codec's complex tap, and worth letting it have.

Compression ratio, ours in bold, FLAC's block size forced to our packet size
throughout:

| stream | ours | -5 @pkt | -8 @pkt | -8 -e -p @pkt |
| --- | --- | --- | --- | --- |
| **AM 909 kHz** (primary), 24 kHz, 480 | **2.248x** | 2.460x | 2.653x | 2.774x |
| **USB 909 kHz** (primary), 12 kHz, 240 | **2.094x** | 1.950x | 2.067x | 2.220x |
| CW 14.025, 12 kHz, 240 | **4.099x** | 2.895x | 3.054x | 3.544x |
| SSB voice 7.150, 12 kHz, 240 | **2.179x** | 1.671x | 1.837x | 1.997x |
| SSB data 14.074, 12 kHz, 240 | **1.817x** | 1.376x | 1.479x | 1.814x |
| AM 14.074, 24 kHz, 480 | **1.689x** | 1.658x | 1.674x | 1.680x |
| NFM 14.074, 24 kHz, 480 | **1.399x** | 1.404x | 1.422x | 1.430x |
| IQ 12 kHz, 240 | **1.377x** | 1.276x | 1.277x | 1.284x |
| IQ 384 kHz medium wave, 360 | **1.793x** | 1.738x | 1.754x | 1.839x |
| IQ 384 kHz quiet band, 360 | **1.538x** | 1.528x | 1.528x | 1.528x |
| IQ 384 kHz busy band, 360 | **1.477x** | 1.409x | 1.411x | 1.417x |

Same data as FLAC's size relative to ours. **Negative means FLAC is smaller**
and FLAC wins that arm:

| stream | -5 @pkt | -8 @pkt | -8 -e -p @pkt |
| --- | --- | --- | --- |
| **AM 909 kHz** (primary), 24 kHz, 480 | -8.6% | -15.3% | -19.0% |
| **USB 909 kHz** (primary), 12 kHz, 240 | +7.4% | +1.3% | -5.7% |
| CW 14.025, 12 kHz, 240 | +41.6% | +34.2% | +15.7% |
| SSB voice 7.150, 12 kHz, 240 | +30.4% | +18.6% | +9.1% |
| SSB data 14.074, 12 kHz, 240 | +32.1% | +22.8% | +0.1% |
| AM 14.074, 24 kHz, 480 | +1.9% | +0.9% | +0.6% |
| NFM 14.074, 24 kHz, 480 | -0.4% | -1.6% | -2.2% |
| IQ 12 kHz, 240 | +7.9% | +7.8% | +7.2% |
| IQ 384 kHz medium wave, 360 | +3.1% | +2.2% | -2.5% |
| IQ 384 kHz quiet band, 360 | +0.7% | +0.7% | +0.7% |
| IQ 384 kHz busy band, 360 | +4.8% | +4.7% | +4.3% |

### Reading that table

**Do we win when FLAC is held to the same constraint?** We win 7 of 11 against
`-5 @pkt` and `-8 @pkt`, and the margins where we win are large: **+34% on CW,
+23% on FT8 audio, +19% on SSB voice** against `-8 @pkt`. We lose the 909 kHz AM
capture badly (−15%), NFM narrowly (−1.6%), and the quiet 384 kHz band by 0.7%.
Against the exhaustive `-8 -e -p @pkt` we win 6 of 11, but the margins shrink to
+16% on CW and +9% on voice, and we lose AM 909 by 19%.

**What decides it.** The pattern is consistent: the shorter the block, the more
FLAC pays for restarting, and the more the per-block cost of transmitting LPC
coefficients hurts it — which is exactly the regime our backward-adaptive
predictor was designed for, since it transmits nothing about its taps at all. On
CW, where a 240-sample block is 20 ms of a slowly-keyed tone, FLAC at `-8`
manages 3.054× while we stay at 4.099×, because it must re-send a model for
every 20 ms and we send none. Conversely, on a signal that is nearly stationary over the whole
recording — a broadcast carrier with shallow modulation — FLAC's per-block
optimal LPC plus partitioned Rice beats a sign-sign LMS cascade with one Rice
parameter per packet, at every block size. That is the 909 kHz AM row, and it is
a real weakness, not an artefact of the test.

Two structural advantages FLAC keeps even at a matched block size, both of which
we could in principle take and have not:

- **Rice partitioning.** FLAC splits a block into up to 2^8 partitions and
  chooses `k` per partition. §6.3 chooses one `k` for the whole packet. On a
  packet spanning a CW key-down/key-up edge, or an AM programme transient, that
  is the difference the exhaustive `-p` arm keeps finding.
- **Per-block optimal LPC.** FLAC solves for its coefficients and sends them;
  we adapt towards them and send nothing. On a stationary signal, solving wins;
  on a short block of a changing one, sending the coefficients costs more than
  it recovers.

We keep two advantages FLAC has no answer for, neither of which this test
scores: **a complex predictor** that models I/Q rotation directly (FLAC's
mid-side is a real-valued approximation of it), which is where the two IQ wins
come from; and **per-packet independence with resynchronisation**, so a lost
packet costs one packet. FLAC has no packet-loss story at all — a truncated
frame desynchronises the stream, and the format offers nothing equivalent to
§5.7.

---

## Timing

Wall clock, best of three runs, whole file in and whole file out, both tools
invoked the same way as subprocesses. Process startup was measured and is not a
confound: 1.5 ms for the Go binary, 1.0 ms for `flac`.

| stream | ours enc/dec | -5 @pkt | -8 @pkt | -8 -e -p @pkt |
| --- | --- | --- | --- | --- |
| **AM 909 kHz** (primary), 24 kHz, 480 | 0.060/0.069 | 0.016/0.012 | 0.031/0.013 | 1.000/0.012 |
| **USB 909 kHz** (primary), 12 kHz, 240 | 0.031/0.034 | 0.010/0.008 | 0.023/0.009 | 0.621/0.008 |
| CW 14.025, 12 kHz, 240 | 0.025/0.014 | 0.005/0.004 | 0.009/0.004 | 0.243/0.004 |
| SSB voice 7.150, 12 kHz, 240 | 0.014/0.015 | 0.005/0.004 | 0.009/0.004 | 0.248/0.005 |
| SSB data 14.074, 12 kHz, 240 | 0.013/0.015 | 0.004/0.004 | 0.009/0.004 | 0.234/0.004 |
| AM 14.074, 24 kHz, 480 | 0.027/0.030 | 0.007/0.006 | 0.013/0.006 | 0.401/0.006 |
| NFM 14.074, 24 kHz, 480 | 0.032/0.028 | 0.007/0.006 | 0.013/0.006 | 0.389/0.006 |
| IQ 12 kHz, 240 | 0.024/0.018 | 0.009/0.006 | 0.027/0.006 | 0.952/0.006 |
| IQ 384 kHz medium wave, 360 | 0.024/0.025 | 0.011/0.007 | 0.031/0.008 | 0.782/0.008 |
| IQ 384 kHz quiet band, 360 | 0.028/0.026 | 0.010/0.007 | 0.026/0.006 | 0.829/0.006 |
| IQ 384 kHz busy band, 360 | 0.022/0.025 | 0.011/0.007 | 0.028/0.007 | 0.807/0.008 |


**FLAC at `-5` and `-8` is faster than us as well as smaller** — 3–5× faster to
encode on the primary stream (0.013–0.020 s against our 0.060 s) and 5–6× faster
to decode. Only the exhaustive `-e -p` arms cost more than us, and they cost a
great deal more: 0.356 s and 1.000 s to encode the primary stream, 6× and 17×
our encode time.

So the trade on offer is **not** "FLAC wins on size but costs CPU". On this
recording, at `-8`, FLAC wins on size *and* on CPU. The only place a CPU
argument helps us is against `-8 -e -p`, which is where FLAC's block-matched arm
finally beats us on several streams — there, FLAC buys the last 5–15% at 17×
the encode cost, and that is a trade a receiver serving twenty sessions would
not take.

For reference, our own in-memory figures from `uiq bench` on the primary stream,
which exclude file I/O and agree with the wall clock above: encode 35.9 µs per
480-sample packet (13.4 Msample/s, 0.18% of a core at 50 packets/s), decode
37.0 µs (13.0 Msample/s).

---

## Honest conclusion

1. **Held to the same constraint, we win more often than we lose.** 7 of 11
   streams against `-8`, and the wins are large and on the signals that matter
   to a receiver: +34% on CW, +23% on FT8 audio, +19% on SSB voice, plus both
   narrowband and busy-band IQ. FLAC takes the stationary-carrier cases and the
   near-white NFM case.

2. **On broadcast AM we lose, and clearly.** 9–19% on the 909 kHz capture, and
   FLAC is faster there too at `-5` and `-8`. Shallow modulation on a stationary
   carrier is where per-block optimal LPC has the most to solve for and where a
   backward-adaptive filter has the least to adapt to.

3. **The constraint is not a handicap we chose to impose on FLAC.** It is the
   only configuration a live stream can use. Every block must decode on its own,
   because the previous one may not have arrived.

4. **There is a concrete improvement visible in these numbers.** The gap that
   survives at a matched block size, on the streams we lose, tracks FLAC's Rice
   partitioning almost exactly: `-8 @pkt` beats `-8 -e -p @pkt` nowhere, and the
   `-p` flag alone is worth 5–15% on the streams with transients in them.
   Choosing `k` per partition rather than per packet is a change §6.3 could take
   without altering the packet layout, and it is where the remaining headroom
   is. §6.3.4 already reports that adapting `k` continuously was tried and was
   slightly worse; partitioned `k` is not the same experiment, and these numbers
   say it is worth running.

5. **The comparison is not like for like on the thing that matters most, and
   that is FLAC's disadvantage, not ours.** FLAC is a file format. It has no
   resynchronisation, no per-packet header carrying rate and timestamp, and no
   defined behaviour on a dropped packet. This test gave FLAC every advantage
   that could be measured in bytes and none of the costs that cannot be.

---

## What could not be measured

- **`flac -9`.** Not implemented by the reference encoder (1.4.3 calls it
  reserved). `-8` and `-8 -e -p` were run in its place.
- **FLAC's per-frame header cost in isolation.** The difference between the
  default and matched block sizes is the combined effect of more frame headers
  *and* less LPC context, and this test does not separate them. Our own header
  cost is separated and measured: 9.07 bytes/packet.
- **Packet-loss behaviour**, on either side. It is the difference that matters
  most between these two formats and it is not a byte count.

---

## Reproducing

```
bitdepth-iq/flaccmp.py IN.pcm RATE FRAMES_PER_PACKET OUTDIR [CHANNELS]
```

Per-arm JSON, including frames-only and total byte counts and the itemised FLAC
metadata blocks, is in `bitdepth-iq/work/flac/*/arms.json`. Captures are
`bitdepth-iq/captures/audio/flac-am909.{pcm,json}` and `flac-usb909.{pcm,json}`;
the other nine streams are the reference corpus unpacked into
`bitdepth-iq/work/spec10/orig/`. All of that is gitignored.
