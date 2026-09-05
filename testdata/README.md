# Conformance vectors

Each vector is a pair of files:

- `NAME.pcm` — raw little-endian `int16` samples, interleaved I/Q for two
  channels. The same layout as the body of a WAV file, so a WAV header can be
  put in front of one without touching the samples.
- `NAME.uiq` — the encoded stream, as length-prefixed packets (see SPEC.md §9).

Ratios below count packet bytes, not the container's 4-byte length prefixes,
since the prefixes exist only because these are files.

| vector | rate | channels | frames/packet | samples | ratio |
|---|---|---|---|---|---|
| `audio-cw-12k` | 12 kHz | 1 | 240 | 36,000 | 4.80× |
| `audio-usb-12k` | 12 kHz | 1 | 240 | 36,000 | 1.66× |
| `iq-12k` | 12 kHz | 2 | 240 | 48,000 | 1.43× |
| `iq-384k` | 384 kHz | 2 | 360 | 57,600 | 1.58× |
| `iq-384k-mw` | 384 kHz | 2 | 360 | 57,600 | 1.58× |
| `rice-63` | 96 kHz | 2 | 240 | 3,840 | 1.32× |
| `edge-cases` | mixed | mixed | mixed | 7,680 | 1.53× |
| `iq-scaled` | 384 kHz | 2 | 360 | 12,240 | 1.71× |

## What each one is for

`audio-cw-12k` — a narrow CW tone in a mostly empty channel. The highest ratio
the format reaches, and the case where the Rice parameter drops to 0.

`audio-usb-12k` — demodulated single sideband carrying data. An ordinary
voice-bandwidth channel, the commonest audio case.

`iq-12k` — narrowband complex baseband. The only IQ rate a browser normally
handles, so the one a JavaScript decoder must get right.

`iq-384k` — wideband complex baseband on a busy amateur band. The throughput
case: at 1099 packets a second this is what decides whether an implementation
keeps up.

`iq-384k-mw` — medium wave, with broadcast carriers about 60 dB above the noise
floor. Its large unpredicted samples produce unary runs long enough to have
exposed a bit-writer overflow that every other capture passed straight through.
Any new implementation should be tested against this one specifically.

`rice-63` — generated rather than captured, for one condition that a capture
will not contain often enough to rely on: a Rice codeword whose unary run is
exactly 63 bits long and is counted out of a full 64-bit accumulator, so
consuming it is a shift by the whole accumulator width (SPEC.md §6.3.3). Go,
Python and JavaScript define that as zero; C and C++ leave it undefined, and on
x86 the accumulator keeps its bits instead, after which the packet decodes to
noise that the backward-adaptive predictor then takes into its filter state —
so the stream stays wrong for the rest of the connection, usually without any
error being raised. It happens about once in a quarter of a million packets on
live 96 kHz IQ. Defined by `riceLongRunStream` in
`go/ubersdriq/vectors_test.go`, which asserts the property is still present, and
regenerated with:

    UIQ_WRITE_VECTORS=1 go -C go test ./ubersdriq -run TestWriteRiceLongRun

`iq-scaled` — generated from the medium-wave capture, for the reduced-depth IQ
profile (SPEC.md §6.6), which none of the vectors above reaches. Medium wave is
the band that mode exists for: carriers 60 dB above the noise are what make a
fixed bit depth unusable and a per-packet one worth having. It covers profile 2
at five shifts including zero, a silent scaled packet — which carries no shift
byte, because it carries no body — an escaped scaled packet, which does carry
one, and crossings out to profile 0 and back, each of which rebuilds the codec.
The shift schedule is part of the vector, not a claim about how a sender should
choose one; choosing a shift is sender policy and nothing on the wire depends on
it. Its `.pcm` holds the restored samples, which are what a decoder must
produce, not the samples that went into the encoder — this is the one vector
where those differ. Defined by `scaledIQStream` in `go/ubersdriq/vectors_test.go`
and regenerated with:

    UIQ_WRITE_VECTORS=1 go -C go test ./ubersdriq -run TestWriteScaledIQ

`edge-cases` — generated rather than captured, because a live capture happens to
contain none of the conditions that are easiest to get wrong. In order: coded
audio, digital silence, coded audio again with filters that advanced across the
silence, full-scale white noise that forces the escape, a sample-count change, a
sample-rate change, a switch to two channels that forces a different predictor
profile, and signal-quality readings that change and then stop changing. It is
defined by `edgeCaseStream` in `go/ubersdriq/vectors_test.go` and regenerated
with:

    UIQ_WRITE_VECTORS=1 go -C go test ./ubersdriq -run TestWriteEdgeCases

## Provenance

The five signal vectors are real traffic from a live receiver, captured on
2026-08-31 over an audio WebSocket and re-encoded with the reference encoder in
`go/`. Synthetic signals cannot replace them: every bug found while developing
this format was triggered by a property of real RF that a generator does not
reproduce — unary runs long enough to overflow a bit writer, band statistics
that decide which predictor configuration wins, and residual distributions that
determine whether the entropy coder is well matched.

They are short — 80 to 150 packets, a few seconds each. That is deliberate,
to keep the repository small, but it means the ratios above are lower than the
format's steady-state figures: a cold predictor costs 10–15% on narrowband
audio over the first few hundred packets (SPEC.md §8.3). The steady-state
figures over 600-packet captures are in SPEC.md §10.1.

It also means none of them is long enough to show what the tap leak of
SPEC.md §6.2.3 buys — that takes minutes of a carrier-dense band — while every
one of them is long enough to show whether it is there, because it changes the
coded bytes from the first packets. The ratios above are what the leaky
predictor achieves on this data; an implementation without the leak would score
slightly higher on `iq-384k-mw` here and would fall apart on the same band after
a minute.

## Both directions are checked

Every implementation decodes each `.uiq` and compares against the `.pcm`. That
is the correctness requirement, and it is what the format demands.

The `edge-cases` and `iq-scaled` vectors are decode-only for the ports: both are
defined by a generator in `go/ubersdriq/vectors_test.go` rather than by encoding
a `.pcm` with fixed parameters, and `iq-scaled` additionally depends on a shift
schedule that is not derivable from its samples.

The Go, Python and C++ implementations additionally encode each of the other
`.pcm` files and require the result to equal the committed `.uiq` **byte for
byte**. The format
does not require that — an encoder may legitimately choose a different Rice
parameter, resynchronise more often, or take the escape earlier, and its output
would still decode correctly. It is required here because it is a far sharper
test of a port: a predictor that is subtly wrong in a way its own decoder
reproduces will round-trip perfectly and still be wrong.

---

This file is licensed under CC BY-SA 4.0 (`../LICENSE-DOCS`).
The vectors themselves are covered by AGPL-3.0-or-later (`../LICENSE`).
