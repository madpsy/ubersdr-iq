// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq_test

import (
	"math"
	"testing"

	"github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

func baseHeader(ts uint64) ubersdriq.Header {
	return ubersdriq.Header{
		TimestampNanos: ts,
		SampleRate:     12000,
		Channels:       1,
		SampleCount:    240,
		BasebandPower:  -42.5,
		Noise:          -95.25,
		Profile:        ubersdriq.ProfileAudio,
	}
}

// Everything that goes in must come out, whether or not it was transmitted.
func TestHeaderRoundTrip(t *testing.T) {
	e := ubersdriq.NewHeaderEncoder()
	d := ubersdriq.NewHeaderDecoder()
	ts := uint64(1_700_000_000_000_000_000)

	for i := 0; i < 200; i++ {
		h := baseHeader(ts)
		switch {
		case i == 50:
			h.SampleCount = 480
		case i == 100:
			h.SampleRate, h.Channels, h.Profile = 384000, 2, ubersdriq.ProfileIQ
		case i == 150:
			h.BasebandPower, h.Noise = -12.0, -80.0
		case i == 160:
			h.Silent = true
		case i == 170:
			h.Escape = true
		}
		pkt := e.Append(nil, h)
		got, off, err := d.Decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if off != len(pkt) {
			t.Fatalf("packet %d: header is %d bytes, decoder stopped at %d", i, len(pkt), off)
		}
		if got != h {
			t.Fatalf("packet %d: got %+v, want %+v", i, got, h)
		}
		ts += 20_000_000
	}
}

// The whole point of the header is that most packets carry almost none of it.
func TestHeaderSteadyStateIsSmall(t *testing.T) {
	e := ubersdriq.NewHeaderEncoder()
	ts := uint64(1_700_000_000_000_000_000)
	var total, n int
	var first int
	for i := 0; i < 250; i++ { // five seconds at 50 packets a second
		pkt := e.Append(nil, baseHeader(ts))
		if i == 0 {
			first = len(pkt)
		}
		total += len(pkt)
		n++
		ts += 20_000_000
	}
	mean := float64(total) / float64(n)
	// The fixed alternative these sizes are compared against is 37 bytes.
	if first < 15 {
		t.Errorf("the first packet should be a full resynchronisation point, got %d bytes", first)
	}
	if mean > 10 {
		t.Errorf("mean header %.2f bytes, expected under 10", mean)
	}
	t.Logf("first packet %d bytes, mean %.2f over %d packets", first, mean, n)
}

// A decoder that opens a stream at an arbitrary offset knows nothing. It must
// refuse to guess, and must recover at the next resynchronisation point.
func TestHeaderMidStreamJoin(t *testing.T) {
	e := ubersdriq.NewHeaderEncoder()
	e.ResyncNanos = 1_000_000_000
	ts := uint64(1_700_000_000_000_000_000)
	var packets [][]byte
	for i := 0; i < 150; i++ {
		packets = append(packets, e.Append(nil, baseHeader(ts)))
		ts += 20_000_000
	}

	d := ubersdriq.NewHeaderDecoder()
	const joinAt = 37
	recovered := -1
	for i := joinAt; i < len(packets); i++ {
		if _, _, err := d.Decode(packets[i]); err == nil {
			recovered = i
			break
		}
	}
	if recovered < 0 {
		t.Fatal("a decoder joining mid-stream never recovered")
	}
	if elapsed := float64(recovered-joinAt) * 0.02; elapsed > 1.05 {
		t.Errorf("recovery took %.2f s, longer than the 1 s resync interval", elapsed)
	}
	t.Logf("joined at packet %d, header recovered at %d", joinAt, recovered)
}

// A timestamp that moves backwards, or jumps further than the resync interval,
// means the stream is not continuous where a delta would assume it was.
func TestHeaderTimestampDiscontinuity(t *testing.T) {
	for _, tc := range []struct {
		name string
		jump func(uint64) uint64
	}{
		{"backwards", func(ts uint64) uint64 { return ts - 5_000_000_000 }},
		{"far forward", func(ts uint64) uint64 { return ts + 60_000_000_000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ubersdriq.NewHeaderEncoder()
			d := ubersdriq.NewHeaderDecoder()
			ts := uint64(1_700_000_000_000_000_000)
			for i := 0; i < 5; i++ {
				if _, _, err := d.Decode(e.Append(nil, baseHeader(ts))); err != nil {
					t.Fatal(err)
				}
				ts += 20_000_000
			}
			ts = tc.jump(ts)
			pkt := e.Append(nil, baseHeader(ts))
			if pkt[4]&ubersdriq.FlagMetadata == 0 {
				t.Error("a discontinuity should force a resynchronisation point")
			}
			got, _, err := d.Decode(pkt)
			if err != nil {
				t.Fatal(err)
			}
			if got.TimestampNanos != ts {
				t.Errorf("timestamp %d, want %d", got.TimestampNanos, ts)
			}
		})
	}
}

func TestQualityMapping(t *testing.T) {
	for _, tc := range []struct {
		in   float32
		want float32
	}{
		{-999, -999},   // the sentinel
		{-1000, -999},  // anything at or below it
		{0, 0},         //
		{-42.5, -42.5}, // ordinary readings survive exactly
		{-95.25, -95.25},
		{327.67, 327.67},   // the top of the range
		{-327.67, -327.67}, // and the bottom
		{500, 327.67},      // outside it, clamped
		{float32(math.Inf(1)), 327.67},
		{float32(math.Inf(-1)), -999},
	} {
		got := ubersdriq.QualityToFloat(ubersdriq.QualityFromFloat(tc.in))
		if got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.in, got, tc.want)
		}
	}
	// A reading that is not a number is not a reading.
	if q := ubersdriq.QualityFromFloat(float32(math.NaN())); q != ubersdriq.QualityNoReading {
		t.Errorf("NaN mapped to %d, want the no-reading codepoint", q)
	}
	// The resolution claim: 0.01 dB, so the error in a derived SNR stays under
	// 0.01 dB.
	for v := -120.0; v < 20; v += 0.013 {
		got := ubersdriq.QualityToFloat(ubersdriq.QualityFromFloat(float32(v)))
		// Half a step is 0.005 dB; the extra slack is the error in representing v
		// as a float32 in the first place.
		if d := math.Abs(float64(got) - v); d > 0.0051 {
			t.Fatalf("%.4f dB round-tripped to %.4f, error %.4f", v, got, d)
		}
	}
}

func TestHeaderRejectsMalformed(t *testing.T) {
	e := ubersdriq.NewHeaderEncoder()
	good := e.Append(nil, baseHeader(1_700_000_000_000_000_000))

	for _, tc := range []struct {
		name string
		pkt  []byte
	}{
		{"empty", nil},
		{"too short", good[:4]},
		{"bad magic", append([]byte{0, 0, 0, 0}, good[4:]...)},
		{"truncated timestamp", good[:8]},
		{"truncated body", good[:len(good)-2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ubersdriq.NewHeaderDecoder()
			if _, _, err := d.Decode(tc.pkt); err == nil {
				t.Error("accepted a malformed header")
			}
		})
	}

	// A packet claiming to be both escape and silent is contradictory.
	bad := append([]byte(nil), good...)
	bad[4] |= ubersdriq.FlagEscape | ubersdriq.FlagSilent
	if _, _, err := ubersdriq.NewHeaderDecoder().Decode(bad); err == nil {
		t.Error("accepted escape and silent together")
	}
}
