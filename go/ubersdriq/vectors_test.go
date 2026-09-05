// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq_test

import (
	"bufio"
	"encoding/binary"
	"math/bits"
	"os"
	"path/filepath"
	"testing"

	"github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

// The edge-case vector.
//
// The five signal vectors are real receiver traffic and exercise the ordinary
// path thoroughly, but a live capture happens not to contain any of the
// conditions that are easiest to get wrong in a port. This builds a stream
// that contains all of them, in one place, in a defined order:
//
//	1. ordinary coded audio, to establish a baseline
//	2. digital silence, which is a header and no body at all
//	3. more coded audio, which must decode correctly with filters that
//	   advanced across the silence
//	4. full-scale white noise, which no predictor can help, so the escape is
//	   taken and the body is verbatim samples
//	5. a change of sample count mid-stream
//	6. a change of sample rate, which forces a resynchronisation point
//	7. a change to two channels, which forces a different predictor profile
//	   and rebuilds the codec
//	8. signal-quality readings that change and then stop changing
//
// A port that decodes this file correctly has handled every branch in the
// header decoder and every mode of the payload codec.
//
// Regenerate with:  UIQ_WRITE_VECTORS=1 go test ./ubersdriq -run TestWriteEdgeCases

// edgeCaseStream is the definition of that stream. Kept as a function so the
// generator and the test that checks the committed file cannot disagree.
func edgeCaseStream(t *testing.T) (packets [][]byte, samples []int16) {
	t.Helper()
	enc := ubersdriq.NewStreamEncoder()
	// A one-second resync interval keeps several resynchronisation points in a
	// short file; the default of five seconds would give exactly one.
	enc.HeaderEncoder().ResyncNanos = 1_000_000_000

	ts := uint64(1_700_000_000_000_000_000)
	emit := func(s []int16, rate, ch int, power, noise float32) {
		pkt, err := enc.Encode(s, ts, rate, ch, power, noise)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), pkt...))
		samples = append(samples, s...)
		ts += uint64(len(s)/ch) * 1e9 / uint64(rate)
	}

	// A deterministic pseudorandom source, so the vector is reproducible
	// anywhere without depending on a language's RNG.
	rnd := uint32(2463534242)
	next := func() uint32 {
		rnd ^= rnd << 13
		rnd ^= rnd >> 17
		rnd ^= rnd << 5
		return rnd
	}

	// 1. A decaying tone plus a little noise: ordinary, compressible audio.
	tone := func(n int, amp float64, period int) []int16 {
		out := make([]int16, n)
		for i := range out {
			// A triangle rather than a sine, so no trigonometry is needed to
			// regenerate this in any language.
			p := i % period
			v := float64(p)/float64(period)*4 - 1
			if p > period/2 {
				v = 3 - float64(p)/float64(period)*4
			}
			out[i] = int16(v*amp) + int16(next()%9) - 4
		}
		return out
	}
	for i := 0; i < 4; i++ {
		emit(tone(240, 8000, 32), 12000, 1, -30.5, -95.25)
	}

	// 2. Digital silence: header only, no body.
	for i := 0; i < 3; i++ {
		emit(make([]int16, 240), 12000, 1, -99.9, -99.9)
	}

	// 3. Signal again, decoded by filters that advanced across the silence.
	for i := 0; i < 3; i++ {
		emit(tone(240, 8000, 32), 12000, 1, -30.5, -95.25)
	}

	// 4. Full-scale white noise: the escape path.
	for i := 0; i < 2; i++ {
		s := make([]int16, 240)
		for j := range s {
			s[j] = int16(next() >> 16)
		}
		emit(s, 12000, 1, -3.0, -6.0)
	}

	// 5. A different sample count, which sends the count field again.
	emit(tone(120, 6000, 16), 12000, 1, -3.0, -6.0)
	emit(tone(360, 6000, 48), 12000, 1, -3.0, -6.0)

	// 6. A sample rate change, which forces a resynchronisation point.
	for i := 0; i < 3; i++ {
		emit(tone(480, 8000, 64), 24000, 1, -20.0, -80.0)
	}

	// 7. Two channels: a different profile, so the codec is rebuilt.
	for i := 0; i < 4; i++ {
		s := make([]int16, 480)
		for j := 0; j < len(s); j += 2 {
			// A rotating phasor approximated with a repeating pattern, which
			// is what a carrier looks like in complex baseband.
			k := (j / 2) % 8
			ir := []int16{9000, 6400, 0, -6400, -9000, -6400, 0, 6400}
			qr := []int16{0, 6400, 9000, 6400, 0, -6400, -9000, -6400}
			s[j] = ir[k] + int16(next()%7) - 3
			s[j+1] = qr[k] + int16(next()%7) - 3
		}
		emit(s, 12000, 2, -25.0, -90.0)
	}

	// 8. Readings that stop changing, so the quality field stops being sent.
	for i := 0; i < 3; i++ {
		emit(tone(240, 4000, 24), 12000, 1, -42.0, -88.0)
	}
	// And one with no reading at all, which is the -999 sentinel.
	emit(tone(240, 4000, 24), 12000, 1, -999, -999)

	return packets, samples
}

// The rice-63 vector.
//
// One condition that no captured signal in this set happens to contain, and
// that a port can get wrong without any other vector noticing: a Rice codeword
// whose unary run is exactly 63 bits long, counted out of a full 64-bit
// accumulator.
//
// A decoder that buffers bits in a 64-bit accumulator refills it while it holds
// 56 bits or fewer, so the accumulator can hold exactly 64. Consuming a 63-bit
// run and its stop bit is then a shift by 64. Go, Python and JavaScript all
// define that as zero. C and C++ do not: it is undefined, and on x86 the shift
// count is taken modulo 64, so the accumulator keeps every bit it had and the
// next refill ORs fresh bytes into stale ones. The rest of the packet decodes
// as noise, and because the predictor is backward adaptive it takes that noise
// into its taps -- so the stream stays wrong for the rest of the connection
// rather than for one packet, usually without raising an error at all.
//
// Measured on live 96 kHz IQ it happens about once every quarter of a million
// packets: often enough to break a receiver within minutes of starting, rare
// enough that a recorded vector would only contain one by luck. This one is
// generated instead, and riceHasFullAccumulatorRun asserts the property is
// still there so it cannot be lost to an innocent change.
//
// Regenerate with:  UIQ_WRITE_VECTORS=1 go test ./ubersdriq -run TestWriteRiceLongRun

// riceHasFullAccumulatorRun re-walks a Rice body the way a decoder does and
// reports whether the unary-run branch ever consumes a full 64 bits at once.
func riceHasFullAccumulatorRun(src []byte, count int) bool {
	if len(src) < 1 {
		return false
	}
	k := uint(src[0])
	if k > 30 {
		return false
	}
	src = src[1:]

	var acc uint64
	var nbits uint
	i := 0
	refill := func() {
		for nbits <= 56 && i < len(src) {
			acc |= uint64(src[i]) << nbits
			i++
			nbits += 8
		}
	}
	refill()

	hit := false
	for j := 0; j < count; j++ {
		if nbits < 48 {
			refill()
		}
		for {
			c := uint(bits.TrailingZeros64(^acc))
			if c < nbits {
				if c+1 >= 64 {
					hit = true
				}
				acc >>= c + 1
				nbits -= c + 1
				break
			}
			if i >= len(src) {
				return hit
			}
			acc, nbits = 0, 0
			refill()
		}
		if nbits < k {
			refill()
		}
		if nbits < k {
			return hit
		}
		acc >>= k
		nbits -= k
	}
	return hit
}

// riceLongRunStream is the definition of that stream.
//
// A strong carrier in complex baseband with a full-scale hit about once in
// sixty frames: the carrier keeps the Rice parameter low and the hits produce
// residuals large enough for a long unary run, which is the combination that
// puts one of them on a full accumulator. The xorshift seed is 2 because that
// is the first one that does; nothing else about it is significant, and the
// assertion below is what makes that reproducible rather than remembered.
func riceLongRunStream(t *testing.T) (packets [][]byte, samples []int16) {
	t.Helper()
	enc := ubersdriq.NewStreamEncoder()
	hdr := ubersdriq.NewHeaderDecoder()

	rnd := uint32(2)
	next := func() uint32 {
		rnd ^= rnd << 13
		rnd ^= rnd >> 17
		rnd ^= rnd << 5
		return rnd
	}

	// A rotating phasor approximated with a repeating pattern, which is what a
	// carrier looks like in complex baseband -- the same shape the edge-case
	// vector uses, at an amplitude that leaves room for the hits.
	ir := []int16{9000, 6400, 0, -6400, -9000, -6400, 0, 6400}
	qr := []int16{0, 6400, 9000, 6400, 0, -6400, -9000, -6400}

	ts := uint64(1_700_000_000_000_000_000)
	found := false
	for p := 0; p < 8; p++ {
		s := make([]int16, 480)
		for j := 0; j < len(s); j += 2 {
			k := (j / 2) % 8
			s[j] = ir[k] + int16(next()%64) - 32
			s[j+1] = qr[k] + int16(next()%64) - 32
			if next()%60 == 0 {
				s[j] = int16(next() >> 16)
				s[j+1] = int16(next() >> 16)
			}
		}
		// The signal vectors all carry the no-reading sentinel, and the
		// byte-for-byte encode check in every port re-encodes them with it, so
		// this has to use it too. Quality itself is what edge-cases exercises.
		pkt, err := enc.Encode(s, ts, 96000, 2, -999, -999)
		if err != nil {
			t.Fatal(err)
		}
		cp := append([]byte(nil), pkt...)
		packets = append(packets, cp)
		samples = append(samples, s...)
		ts += uint64(len(s)/2) * 1e9 / 96000

		h, off, err := hdr.Decode(cp)
		if err != nil {
			t.Fatal(err)
		}
		if !h.Silent && !h.Escape && riceHasFullAccumulatorRun(cp[off:], h.SampleCount) {
			found = true
		}
	}
	if !found {
		t.Fatal("rice-63: no codeword consumes a full 64-bit accumulator, so this vector " +
			"no longer tests what it exists for -- search for a new xorshift seed")
	}
	return packets, samples
}

// The scaled-IQ vector.
//
// The reduced-depth IQ profile (SPEC.md section 6.6) is the one part of the
// format a decoder cannot exercise from any of the vectors above: it needs the
// shift byte at the front of the body, the rule that a silent packet does not
// carry one, and the saturating left shift on the way out. Those are three
// small pieces of code that a port can get wrong in ways nothing else notices.
//
// The source is the medium-wave capture, because that is the band the mode
// exists for -- carriers 60 dB above the noise are what make a fixed bit depth
// unusable and a per-packet one worth having. The shift schedule is arbitrary
// and is part of the vector, not a claim about how a sender should choose one;
// see scaled.go.
//
// In order: several packets at one shift, a silent packet on the scaled
// profile (which carries no shift byte at all), a change of shift, a crossing
// out to the lossless profile and back -- each of which rebuilds the codec --
// a shift of zero, full-scale noise at a shift of zero so the escape is taken
// on a scaled packet, and a large shift.
//
// Regenerate with:  UIQ_WRITE_VECTORS=1 go test ./ubersdriq -run TestWriteScaledIQ

// scaledIQStream is the definition of that stream.
func scaledIQStream(t *testing.T) (packets [][]byte, samples []int16) {
	t.Helper()
	src := readPCM(t, vectorPath("iq-384k-mw", ".pcm"))

	enc := ubersdriq.NewStreamEncoder()
	dec := ubersdriq.NewStreamDecoder()
	const per = 720 // 360 I/Q frames, the rate's usual packet
	ts := uint64(1_700_000_000_000_000_000)

	// A deterministic pseudorandom source for the noise packet, so the vector
	// is reproducible anywhere without depending on a language's RNG.
	rnd := uint32(2463534242)
	next := func() uint32 {
		rnd ^= rnd << 13
		rnd ^= rnd >> 17
		rnd ^= rnd << 5
		return rnd
	}

	// emit codes one packet and records what a conforming decoder must produce
	// from it, which is the .pcm side of the pair. Decoding here rather than
	// recomputing the quantisation is deliberate: it means the two halves of
	// the vector cannot disagree.
	emit := func(s []int16, scaled bool, shift uint) {
		var pkt []byte
		var err error
		if scaled {
			pkt, err = enc.EncodeScaled(s, ts, 384000, 2, -999, -999, shift)
		} else {
			pkt, err = enc.Encode(s, ts, 384000, 2, -999, -999)
		}
		if err != nil {
			t.Fatal(err)
		}
		cp := append([]byte(nil), pkt...)
		_, out, err := dec.Decode(cp)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, cp)
		samples = append(samples, out...)
		ts += uint64(len(s)/2) * 1e9 / 384000
	}

	at := 0
	chunk := func() []int16 {
		s := src[at : at+per]
		at += per
		return s
	}

	for i := 0; i < 4; i++ {
		emit(chunk(), true, 3)
	}
	// Digital silence on the scaled profile: no body, and therefore no shift.
	emit(make([]int16, per), true, 3)
	for i := 0; i < 3; i++ {
		emit(chunk(), true, 5)
	}
	// Out to the lossless profile and back, which rebuilds the codec twice.
	for i := 0; i < 2; i++ {
		emit(chunk(), false, 0)
	}
	for i := 0; i < 3; i++ {
		emit(chunk(), true, 2)
	}
	// A shift of zero: still a scaled packet, still carrying its shift byte.
	emit(chunk(), true, 0)
	// Full-scale noise at a shift of zero, which no predictor can help, so the
	// escape is taken -- with the shift byte still ahead of the verbatim body.
	noise := make([]int16, per)
	for j := range noise {
		noise[j] = int16(next() >> 16)
	}
	emit(noise, true, 0)
	for i := 0; i < 2; i++ {
		emit(chunk(), true, 7)
	}
	return packets, samples
}

func TestWriteScaledIQ(t *testing.T) {
	if os.Getenv("UIQ_WRITE_VECTORS") == "" {
		t.Skip("set UIQ_WRITE_VECTORS=1 to regenerate testdata/iq-scaled.*")
	}
	packets, samples := scaledIQStream(t)
	writeVector(t, "iq-scaled", packets, samples)
}

func TestWriteRiceLongRun(t *testing.T) {
	if os.Getenv("UIQ_WRITE_VECTORS") == "" {
		t.Skip("set UIQ_WRITE_VECTORS=1 to regenerate testdata/rice-63.*")
	}
	packets, samples := riceLongRunStream(t)
	writeVector(t, "rice-63", packets, samples)
}

func TestWriteEdgeCases(t *testing.T) {
	if os.Getenv("UIQ_WRITE_VECTORS") == "" {
		t.Skip("set UIQ_WRITE_VECTORS=1 to regenerate testdata/edge-cases.*")
	}
	packets, samples := edgeCaseStream(t)
	writeVector(t, "edge-cases", packets, samples)
}

// writeVector commits one vector as the .uiq/.pcm pair testdata/README.md
// describes.
func writeVector(t *testing.T, name string, packets [][]byte, samples []int16) {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata")
	f, err := os.Create(filepath.Join(dir, name+".uiq"))
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	sw := ubersdriq.NewStreamWriter(bw)
	for _, p := range packets {
		if err := sw.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	raw := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(raw[2*i:], uint16(v))
	}
	if err := os.WriteFile(filepath.Join(dir, name+".pcm"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: %d packets, %d samples", name, len(packets), len(samples))
}
