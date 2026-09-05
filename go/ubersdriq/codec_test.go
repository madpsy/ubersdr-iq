// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq_test

import (
	"math"
	"testing"

	"github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

// xorshift is a deterministic source, so a failure is reproducible and a port
// can generate the same signal.
type xorshift uint32

func (x *xorshift) next() uint32 {
	*x ^= *x << 13
	*x ^= *x >> 17
	*x ^= *x << 5
	return uint32(*x)
}

func newRand() *xorshift { x := xorshift(2463534242); return &x }

// roundTrip encodes each packet and decodes it with a SEPARATE codec, as the
// two ends of a real stream are, and requires bit equality throughout. That
// the two independent adaptations stay in step over a whole stream is the
// property being tested, not just that one packet survives.
func roundTrip(t *testing.T, profile byte, packets [][]int16) (raw, coded int) {
	t.Helper()
	enc, err := ubersdriq.NewCodec(profile)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := ubersdriq.NewCodec(profile)
	if err != nil {
		t.Fatal(err)
	}
	for n, in := range packets {
		body, escape, err := enc.Encode(in)
		if err != nil {
			t.Fatalf("packet %d: encode: %v", n, err)
		}
		out, err := dec.Decode(body, len(in), escape)
		if err != nil {
			t.Fatalf("packet %d: decode: %v", n, err)
		}
		for i := range in {
			if out[i] != in[i] {
				t.Fatalf("packet %d sample %d: got %d, want %d -- not lossless", n, i, out[i], in[i])
			}
		}
		raw += len(in) * 2
		coded += len(body)
	}
	return raw, coded
}

func TestLosslessOnSyntheticSignals(t *testing.T) {
	rng := newRand()
	cases := []struct {
		name     string
		profile  byte
		gen      func(i int) []int16
		minRatio float64
	}{
		{
			// A pure tone is the easiest possible case for a predictor.
			name: "tone", profile: ubersdriq.ProfileAudio, minRatio: 4.0,
			gen: func(n int) []int16 {
				s := make([]int16, 240)
				for i := range s {
					// Phase continues across packets, as a real tone does.
					s[i] = int16(12000 * math.Sin(float64(n*240+i)*0.07))
				}
				return s
			},
		},
		{
			// Full-scale white noise is the hardest: no predictor can help, so
			// the escape must be taken and the result must still be exact.
			name: "white noise", profile: ubersdriq.ProfileAudio, minRatio: 0.99,
			gen: func(int) []int16 {
				s := make([]int16, 240)
				for i := range s {
					s[i] = int16(rng.next() >> 16)
				}
				return s
			},
		},
		{
			name: "digital silence", profile: ubersdriq.ProfileAudio, minRatio: 7.0,
			gen: func(int) []int16 { return make([]int16, 240) },
		},
		{
			// Alternating extremes: the largest possible sample-to-sample
			// steps, which produce the largest residuals a 16-bit input can.
			name: "full-scale square", profile: ubersdriq.ProfileAudio, minRatio: 0.99,
			gen: func(int) []int16 {
				s := make([]int16, 240)
				for i := range s {
					if i%2 == 0 {
						s[i] = math.MaxInt16
					} else {
						s[i] = math.MinInt16
					}
				}
				return s
			},
		},
		{
			// A rotating phasor: a carrier in complex baseband, which one
			// complex tap cancels. It does not reach the ratio the quantisation
			// floor alone would suggest, and the reason is worth knowing: a
			// sign-sign update has a steady-state misadjustment proportional to
			// the step size and the signal amplitude, so the taps dither around
			// the optimum rather than settling on it. On a pure tone at nearly
			// full scale that dither is the whole residual. Real signals are
			// not pure tones, which is why the same step size measures best on
			// live captures.
			name: "iq carrier", profile: ubersdriq.ProfileIQ, minRatio: 2.0,
			gen: func(n int) []int16 {
				s := make([]int16, 480)
				for i := 0; i < len(s); i += 2 {
					p := float64(n*240+i/2) * 0.11
					s[i] = int16(11000 * math.Cos(p))
					s[i+1] = int16(11000 * math.Sin(p))
				}
				return s
			},
		},
		{
			name: "iq noise", profile: ubersdriq.ProfileIQ, minRatio: 0.99,
			gen: func(int) []int16 {
				s := make([]int16, 480)
				for i := range s {
					s[i] = int16(rng.next() >> 16)
				}
				return s
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var packets [][]int16
			for i := 0; i < 400; i++ {
				packets = append(packets, c.gen(i))
			}
			raw, coded := roundTrip(t, c.profile, packets)
			ratio := float64(raw) / float64(coded)
			t.Logf("%d -> %d bytes, %.3fx", raw, coded, ratio)
			if ratio < c.minRatio {
				t.Errorf("ratio %.3fx, expected at least %.3fx", ratio, c.minRatio)
			}
		})
	}
}

// The escape is the guarantee that the body never expands. Nothing a 16-bit
// input can do may produce a body larger than the samples.
func TestBodyNeverExpands(t *testing.T) {
	rng := newRand()
	for _, profile := range []byte{ubersdriq.ProfileAudio, ubersdriq.ProfileIQ} {
		enc, err := ubersdriq.NewCodec(profile)
		if err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 200; n++ {
			s := make([]int16, 480)
			for i := range s {
				switch n % 4 {
				case 0: // uniform noise over the whole range
					s[i] = int16(rng.next() >> 16)
				case 1: // extremes only
					if rng.next()&1 == 0 {
						s[i] = math.MaxInt16
					} else {
						s[i] = math.MinInt16
					}
				case 2: // sparse impulses on a quiet floor
					if rng.next()%97 == 0 {
						s[i] = math.MinInt16
					} else {
						s[i] = int16(rng.next()%7) - 3
					}
				case 3: // a ramp that wraps, so every value appears
					s[i] = int16(i*137 + n*31)
				}
			}
			body, _, err := enc.Encode(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(body) > len(s)*2 {
				t.Fatalf("profile %d packet %d: body is %d bytes for %d bytes of samples",
					profile, n, len(body), len(s)*2)
			}
		}
	}
}

// A silent packet carries no body, and the predictor must move exactly as if
// it had coded the zeros -- otherwise every packet after it decodes wrongly.
func TestSilenceAdvancesIdentically(t *testing.T) {
	zeros := make([]int16, 240)
	tone := make([]int16, 240)
	for i := range tone {
		tone[i] = int16(9000 * math.Sin(float64(i)*0.05))
	}

	// One codec codes the zeros the ordinary way; the other skips the
	// bitstream. Both must then predict the following packet identically.
	coded, _ := ubersdriq.NewCodec(ubersdriq.ProfileAudio)
	skipped, _ := ubersdriq.NewCodec(ubersdriq.ProfileAudio)
	for i := 0; i < 5; i++ {
		if _, _, err := coded.Encode(tone); err != nil {
			t.Fatal(err)
		}
		if _, _, err := skipped.Encode(tone); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, _, err := coded.Encode(zeros); err != nil {
			t.Fatal(err)
		}
		if err := skipped.AdvanceSilence(len(zeros)); err != nil {
			t.Fatal(err)
		}
	}
	a, _, err := coded.Encode(tone)
	if err != nil {
		t.Fatal(err)
	}
	aCopy := append([]byte(nil), a...)
	b, _, err := skipped.Encode(tone)
	if err != nil {
		t.Fatal(err)
	}
	if string(aCopy) != string(b) {
		t.Fatal("advancing over silence without coding it left the filters in a different state")
	}
}

// An unknown profile is refused. Falling back to a default would decode a
// stream with the wrong predictor and return plausible-sounding noise, which
// is worse than any error.
func TestUnknownProfileIsRefused(t *testing.T) {
	for _, id := range []byte{3, 4, 7} {
		if _, err := ubersdriq.NewCodec(id); err == nil {
			t.Errorf("profile %d was accepted", id)
		}
	}
}

func TestCodecRejectsBadInput(t *testing.T) {
	iq, _ := ubersdriq.NewCodec(ubersdriq.ProfileIQ)
	if _, _, err := iq.Encode(make([]int16, 5)); err == nil {
		t.Error("a complex profile accepted an odd number of samples")
	}
	if _, _, err := iq.Encode(nil); err == nil {
		t.Error("an empty packet was accepted")
	}
	audio, _ := ubersdriq.NewCodec(ubersdriq.ProfileAudio)
	body, escape, err := audio.Encode(make([]int16, 240))
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := ubersdriq.NewCodec(ubersdriq.ProfileAudio)
	if _, err := dec.Decode(body, 0, escape); err == nil {
		t.Error("a zero sample count was accepted")
	}
	if _, err := dec.Decode(nil, 240, false); err == nil {
		t.Error("an empty body was accepted")
	}
	if _, err := dec.Decode(body[:1], 240, true); err == nil {
		t.Error("a truncated escape body was accepted")
	}
}

// A packet lost between encoder and decoder is unrecoverable, and a decoder
// that joins a stream late never produces correct samples. This is a property
// of the design, not a defect, and it is asserted here so that a change which
// silently altered it would be noticed.
func TestStreamHasNoRandomAccess(t *testing.T) {
	rng := newRand()
	var packets [][]int16
	for i := 0; i < 60; i++ {
		s := make([]int16, 240)
		for j := range s {
			s[j] = int16(9000*math.Sin(float64(i*240+j)*0.03)) + int16(rng.next()%64) - 32
		}
		packets = append(packets, s)
	}
	enc, _ := ubersdriq.NewCodec(ubersdriq.ProfileAudio)
	var bodies [][]byte
	var escapes []bool
	for _, p := range packets {
		b, e, err := enc.Encode(p)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, append([]byte(nil), b...))
		escapes = append(escapes, e)
	}

	dec, _ := ubersdriq.NewCodec(ubersdriq.ProfileAudio)
	var wrong, total int
	for i := 30; i < len(bodies); i++ {
		out, err := dec.Decode(bodies[i], len(packets[i]), escapes[i])
		if err != nil {
			t.Fatal(err)
		}
		for j := range out {
			if out[j] != packets[i][j] {
				wrong++
			}
			total++
		}
	}
	if wrong == 0 {
		t.Fatal("a decoder joining mid-stream reconstructed the samples, which the design does not allow for")
	}
	t.Logf("joining at packet 30: %d of %d samples wrong (%.1f%%)",
		wrong, total, 100*float64(wrong)/float64(total))
}
