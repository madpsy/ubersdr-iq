// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

import (
	"math"
	"math/rand"
	"testing"
)

// withSIMD runs fn with the SIMD kernel on or off, restoring it after.
func withSIMD(t testing.TB, on bool, fn func()) {
	t.Helper()
	saved := useSIMD
	useSIMD = on
	defer func() { useSIMD = saved }()
	fn()
}

// kernels lists the block paths this machine can run: always the pure-Go
// one, and the SIMD kernel where there is one.
func kernels() []bool {
	if useSIMD {
		return []bool{false, true}
	}
	return []bool{false}
}

func cloneStage(f *complexStage) *complexStage {
	c := *f
	c.wr = append([]int64(nil), f.wr...)
	c.wi = append([]int64(nil), f.wi...)
	c.hr = append([]int64(nil), f.hr...)
	c.hi = append([]int64(nil), f.hi...)
	c.sr = append([]int64(nil), f.sr...)
	c.si = append([]int64(nil), f.si...)
	return &c
}

// sameStage compares everything a stage's next frame depends on: taps, the
// active window and its signs. Where the window sits in the backing array is
// not compared; the block paths slide it at different times.
func sameStage(a, b *complexStage) bool {
	for j := 0; j < 16; j++ {
		if a.wr[j] != b.wr[j] || a.wi[j] != b.wi[j] {
			return false
		}
		ia, ib := a.idx-16+j, b.idx-16+j
		if a.hr[ia] != b.hr[ib] || a.hi[ia] != b.hi[ib] ||
			a.sr[ia] != b.sr[ib] || a.si[ia] != b.si[ib] {
			return false
		}
	}
	return true
}

// randomStage returns an order-16 stage at rest, at the clamp limit, or with
// taps anywhere in range.
func randomStage(rng *rand.Rand, kind int) *complexStage {
	f := newComplexStage(16, 16)
	for j := 0; j < 16; j++ {
		switch kind {
		case 1:
			v := int64(tapLimit - rng.Intn(64))
			if rng.Intn(2) == 0 {
				v = -v
			}
			f.wr[j], f.wi[j] = v, -v
		case 2:
			f.wr[j] = rng.Int63n(2*tapLimit+1) - tapLimit
			f.wi[j] = rng.Int63n(2*tapLimit+1) - tapLimit
		}
	}
	return f
}

// TestBlockMatchesPerFrame drives forward and inverse frame by frame and each
// block path from the same start, over packets from one frame up and from
// silence to full scale, with taps at rest, at the clamp limit and anywhere
// in range. Residuals or samples, and the stage, must agree after every
// packet. The inverse side also feeds residuals near +-2^31, which take a
// reconstructed sample past int32 and the SIMD kernel's early stop.
func TestBlockMatchesPerFrame(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	for trial := 0; trial < 60; trial++ {
		start := randomStage(rng, trial%3)
		for _, simd := range kernels() {
			want, got := cloneStage(start), cloneStage(start)
			withSIMD(t, simd, func() {
				for p := 0; p < 40; p++ {
					frames := 1 + rng.Intn(400)
					inverse := rng.Intn(2) == 0
					want.beginPacket(frames)
					got.beginPacket(frames)
					if !inverse {
						amp := []int{0, 1, 3, 300, 32768}[rng.Intn(5)]
						in := make([]int16, 2*frames)
						for i := range in {
							if amp == 32768 {
								in[i] = int16(rng.Intn(65536) - 32768)
							} else if amp > 0 {
								in[i] = int16(rng.Intn(2*amp+1) - amp)
							}
						}
						a, b := make([]int32, len(in)), make([]int32, len(in))
						for i := 0; i < len(in); i += 2 {
							r, q := want.forward(int64(in[i]), int64(in[i+1]))
							a[i], a[i+1] = int32(r), int32(q)
						}
						got.forwardBlock16(in, b)
						for i := range a {
							if a[i] != b[i] {
								t.Fatalf("trial %d simd=%v packet %d forward: residual %d is %d, want %d", trial, simd, p, i, b[i], a[i])
							}
						}
					} else {
						in := make([]int32, 2*frames)
						huge := rng.Intn(4) == 0
						for i := range in {
							switch {
							case huge && rng.Intn(50) == 0:
								in[i] = int32(1<<31 - 1 - rng.Intn(1000))
								if rng.Intn(2) == 0 {
									in[i] = -in[i] - 1
								}
							default:
								in[i] = int32(rng.Intn(2001) - 1000)
							}
						}
						a, b := make([]int16, len(in)), make([]int16, len(in))
						for i := 0; i < len(in); i += 2 {
							r, q := want.inverse(int64(in[i]), int64(in[i+1]))
							a[i], a[i+1] = int16(r), int16(q)
						}
						got.inverseBlock16(in, b)
						for i := range a {
							if a[i] != b[i] {
								t.Fatalf("trial %d simd=%v packet %d inverse: sample %d is %d, want %d", trial, simd, p, i, b[i], a[i])
							}
						}
					}
					if !sameStage(want, got) {
						t.Fatalf("trial %d simd=%v packet %d (inverse=%v): stage state differs", trial, simd, p, inverse)
					}
				}
			})
		}
	}
}

// benchIQ384 is 64 packets of 360 I/Q frames, the size of a 384 kHz stream's
// packets: a tone plus noise, since the kernels' cost does not depend on the
// signal.
func benchIQ384() [][]int16 {
	rng := rand.New(rand.NewSource(1))
	pkts := make([][]int16, 64)
	ph := 0.0
	for p := range pkts {
		s := make([]int16, 720)
		for i := 0; i < len(s); i += 2 {
			ph += 0.37
			s[i] = int16(8000*math.Cos(ph)) + int16(rng.Intn(201)-100)
			s[i+1] = int16(8000*math.Sin(ph)) + int16(rng.Intn(201)-100)
		}
		pkts[p] = s
	}
	return pkts
}

func benchmarkBlock(b *testing.B, simd bool, decode bool) {
	pkts := benchIQ384()
	withSIMD(b, simd, func() {
		enc, _ := NewCodec(ProfileIQ)
		dec, _ := NewCodec(ProfileIQ)
		type coded struct {
			body []byte
			esc  bool
		}
		bodies := make([]coded, len(pkts))
		for i, p := range pkts {
			body, esc, err := enc.Encode(p)
			if err != nil {
				b.Fatal(err)
			}
			bodies[i] = coded{append([]byte(nil), body...), esc}
		}
		b.SetBytes(720 * 2)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			k := i % len(pkts)
			if decode {
				if k == 0 {
					dec, _ = NewCodec(ProfileIQ)
				}
				if _, err := dec.Decode(bodies[k].body, 720, bodies[k].esc); err != nil {
					b.Fatal(err)
				}
			} else if _, _, err := enc.Encode(pkts[k]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkEncodeIQ(b *testing.B)       { benchmarkBlock(b, useSIMD, false) }
func BenchmarkDecodeIQ(b *testing.B)       { benchmarkBlock(b, useSIMD, true) }
func BenchmarkEncodeIQPureGo(b *testing.B) { benchmarkBlock(b, false, false) }
func BenchmarkDecodeIQPureGo(b *testing.B) { benchmarkBlock(b, false, true) }
