// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

// Adaptive prediction.
//
// Each sample is predicted from the ones before it by an FIR filter whose taps
// adapt as the stream runs. Only the prediction error is transmitted.
//
// The filter is BACKWARD adaptive: its taps are a function of samples that
// have already been coded, so the decoder recomputes them from its own
// reconstruction and no coefficients are ever sent. The alternative,
// forward-adaptive linear prediction (a Levinson fit per packet with the
// coefficients in the packet), measured worse -- 1.29x against 1.32x on IQ --
// because refitting per packet loses the cross-packet adaptation and the
// coefficients cost bytes on every packet.
//
// The update is leaky sign-sign LMS:
//
//	w[j] += mu * sgn(e) * sgn(x[n-order+j]) - leak(w[j])
//
// rather than true NLMS. It needs only the signs of the error and of the
// history, so it costs one multiply-free step per tap with no division and no
// normalisation, and -- the point -- it is exactly reproducible in integers.
// Measured compression is within a fraction of a percent of a floating-point
// NLMS of the same order. The leak is what stops the taps walking; see
// leakShiftComplex below for why it is not optional.
//
// EVERY ARITHMETIC DETAIL HERE IS LOAD-BEARING. The two ends never exchange
// filter state, so they agree only by computing the same thing: the rounding
// of the prediction sum, the sign convention, the tap clamp, the order in
// which cascade stages are applied, and the point at which the fast path skips
// the clamp. A difference in any of them does not fail loudly. It returns
// plausible-sounding noise.

const (
	// tapShift is the fixed-point scale of the filter taps: they are integers
	// in Q16, so 65536 represents a tap of 1.0.
	tapShift = 16

	// tapLimit bounds |tap| to 2^24, a real-valued magnitude of 256.
	//
	// It serves two purposes. It caps the prediction sum far below int64
	// overflow no matter what the input does: an order-16 filter at the limit
	// against full-scale samples reaches only about 2^45. And because that
	// bound keeps every product and sum inside the range where a float64
	// represents integers exactly (2^53), a JavaScript decoder can use plain
	// numbers and still be bit-exact, with no BigInt.
	//
	// With the leak below holding the taps near their equilibrium the clamp is
	// insurance that never fires in practice. It must nonetheless be applied
	// identically on both sides, because if it ever does fire the two must
	// agree.
	tapLimit = 1 << 24

	// leakShiftComplex and leakShiftReal are the leakage of the tap update:
	// every adapt subtracts w/2^shift from each tap alongside the gradient
	// step.
	//
	// WHY LEAKAGE IS NOT OPTIONAL
	// ---------------------------
	// Sign-sign LMS has no restoring force of its own. The step is
	// mu*sgn(e)*sgn(x) whatever the taps already are, so in any direction the
	// input does not excite -- and a band whose energy sits in a few carriers
	// leaves most directions unexcited -- the taps are free to walk. On medium
	// wave they walk in one direction: measured on a 909 kHz carrier in a
	// 384 kHz baseband, the mean |tap| grew linearly at about 1.5 per second
	// until it reached tapLimit around ninety seconds in.
	//
	// Long before the clamp the prediction is worthless. That stream cost
	// 10.65 bits per sample at five seconds, 14.26 at thirty and 15.85 at
	// ninety, by which point a third of packets were taking the escape and the
	// "compressed" stream was larger than the samples going into it. What a
	// client sees is a bitrate that climbs for minutes on a signal that is not
	// changing. Every vector in testdata/ is a few seconds long, which is why
	// none of them catches it.
	//
	// Subtracting w/2^shift bounds the walk at roughly mu*2^shift. Where the
	// taps were not walking it costs nothing -- on two minutes of a 40 m
	// capture it changes the coded size by 0.003%, in its favour -- and where
	// they were it is decisive: the medium-wave stream holds 8.45 bits per
	// sample for as long as it runs.
	//
	// WHY THE TWO FORMS DIFFER
	// ------------------------
	// The complex filter tracks a 384 kHz baseband where the useful taps are
	// small, and 14 is its measured optimum -- bits per sample over the last
	// five seconds of that ninety-second stream:
	//
	//	 8   13.35      14    8.45      18   11.19
	//	10    9.41      16    9.36      off  15.85
	//	12    8.81
	//
	// The real cascade is a different filter on a different signal: four
	// stages over a 12 kHz channel about four times oversampled, whose taps
	// legitimately reach 1.2. Leaking it as hard as the complex one throws
	// that away. Measured on a 14.074 MHz USB capture, payload ratio over the
	// capture against the largest |tap| after 150 million samples:
	//
	//	14   1.671x  0.90      17   1.906x  1.99
	//	16   1.874x  1.57      off  1.905x  5.33
	//
	// 16 and 17 are within 0.14% of each other summed over five audio
	// captures, so the choice between them is not a compression one. 16 is
	// taken because it is the one that can be TESTED: a tap has to pass 1.0
	// before it leaks at all, which every audio capture does within a second,
	// where at 17 nothing leaks until a stream has run for tens of millions of
	// samples. A port that got this constant wrong, or left the leak out of
	// its real cascade entirely, would pass every vector here at 17 and fails
	// at 16.
	leakShiftComplex = 14
	leakShiftReal    = 16
)

// sgn is a branchless sign, returning -1, 0 or +1.
func sgn(v int64) int64 {
	return (v >> 63) | int64(uint64(-v)>>63)
}

// roundShift divides by 2^shift, rounding to nearest and away from zero on
// ties.
//
// A plain arithmetic shift would round negative values towards negative
// infinity, which biases the predictor; more importantly, both ends must round
// identically, so this is the single definition every implementation copies.
//
// Branchless on purpose: the sign of a prediction sum is close to a coin flip,
// so a branch here mispredicts constantly -- it measured 8% of encode time.
// The mask form computes round(|v|) and restores the sign, which is the same
// value the branchy form produced for every input the filters can generate.
func roundShift(v int64, shift uint) int64 {
	m := v >> 63
	r := (((v ^ m) - m) + 1<<(shift-1)) >> shift
	return (r ^ m) - m
}

// leak is the amount the leakage removes from one tap: the magnitude divided
// by 2^shift and truncated towards zero.
//
// Truncating rather than rounding is what keeps the leak from fighting the
// gradient at small taps. Below 2^shift it returns zero, so a tap the signal
// genuinely wants at a small value stays there instead of being pulled towards
// zero and pushed back every sample.
//
// Branchless in the same form as roundShift, and for the same reason: it runs
// once per tap per sample, and the sign of a tap is not predictable.
//
// A port must not write this as an arithmetic right shift. On a negative tap
// that floors rather than truncating, which is a different value, and in
// JavaScript >> is a 32-bit operator where taps reach past 2^31. Divide the
// magnitude and restore the sign.
func leak(w int64, shift uint) int64 {
	m := w >> 63
	return ((((w ^ m) - m) >> shift) ^ m) - m
}

func clampTap(w int64) int64 {
	if w > tapLimit {
		return tapLimit
	}
	if w < -tapLimit {
		return -tapLimit
	}
	return w
}

// historyLen sizes the sliding history window for a given filter order.
//
// History is linear rather than circular so the tap loops walk contiguous
// memory with no index wrapping, which matters at 1099 packets a second. The
// cost is periodically sliding the newest `order` entries back to the front;
// making the window several times the order amortises that to negligible.
//
// The window size is an implementation choice and not part of the format: it
// changes nothing an implementation transmits. A port is free to use a
// circular buffer instead.
func historyLen(order int) int {
	if n := order * 8; n > 64 {
		return n
	}
	return 64
}

// complexStage is one adaptive complex filter, for interleaved I/Q.
type complexStage struct {
	order int
	mu    int64

	// Taps in Q16, stored oldest-first: wr[j] weighs the history sample at
	// hr[idx-order+j], so predict and adapt walk taps and history forward
	// together and the compiler can drop the per-element bounds checks.
	wr, wi []int64

	// fast is true while this packet provably cannot drive any tap past
	// tapLimit, letting adapt skip the clamp. See beginPacket.
	fast bool

	// History of reconstructed samples, with their signs kept alongside so the
	// update loop does not recompute a sign per tap per sample. The newest
	// entry is at idx-1.
	hr, hi []int64
	sr, si []int64
	idx    int
}

func newComplexStage(order int, mu int64) *complexStage {
	n := historyLen(order)
	return &complexStage{
		order: order, mu: mu,
		wr: make([]int64, order), wi: make([]int64, order),
		hr: make([]int64, n), hi: make([]int64, n),
		sr: make([]int64, n), si: make([]int64, n),
		idx: order,
	}
}

func (f *complexStage) predict() (int64, int64) {
	wr := f.wr
	lo := f.idx - len(wr)
	hr := f.hr[lo:f.idx]
	hr = hr[:len(wr)]
	hi := f.hi[lo:f.idx]
	hi = hi[:len(wr)]
	wi := f.wi[:len(wr)]
	var pr, pi int64
	for j, w := range wr {
		br, bi := hr[j], hi[j]
		wiv := wi[j]
		pr += w*br - wiv*bi
		pi += w*bi + wiv*br
	}
	return roundShift(pr, tapShift), roundShift(pi, tapShift)
}

// adapt nudges each tap by mu in the direction that would have reduced this
// error, after leaking leakShiftComplex off it. The conjugate of the history is
// used, as the complex LMS gradient requires; here that is simply the negated
// sign of the imaginary part.
//
// The early return on a zero error is REQUIRED, not an optimisation. Without
// the leak both steps would be zero and the call would be a no-op either way;
// with it, running the loop would leak the taps towards zero on every silent
// sample. Because the prediction over an all-zero history is zero, the error
// over digital silence is zero, so this is exactly the branch that freezes the
// taps across a squelched run. A port that adds the leak but drops the early
// return parts company with the encoder on the first silent packet.
func (f *complexStage) adapt(er, ei int64) {
	if er == 0 && ei == 0 {
		return
	}
	mr := f.mu * sgn(er)
	mi := f.mu * sgn(ei)
	wr := f.wr
	lo := f.idx - len(wr)
	sr := f.sr[lo:f.idx]
	sr = sr[:len(wr)]
	si := f.si[lo:f.idx]
	si = si[:len(wr)]
	wi := f.wi[:len(wr)]
	if f.fast {
		for j := range wr {
			hrs := sr[j]
			his := -si[j]
			wr[j] += mr*hrs - mi*his - leak(wr[j], leakShiftComplex)
			wi[j] += mr*his + mi*hrs - leak(wi[j], leakShiftComplex)
		}
		return
	}
	for j := range wr {
		hrs := sr[j]
		his := -si[j]
		wr[j] = clampTap(wr[j] + mr*hrs - mi*his - leak(wr[j], leakShiftComplex))
		wi[j] = clampTap(wi[j] + mr*his + mi*hrs - leak(wi[j], leakShiftComplex))
	}
}

// beginPacket decides, once per packet, whether adapt may skip the tap clamp.
//
// One complex update moves a tap by at most 2*mu -- each of the two sign terms
// contributes at most mu, and the leak only ever moves a tap towards zero --
// so if every tap starts further than 2*mu*steps from the limit, no update in
// this packet can reach it and the clamp is an identity. Taps settle around 2^16 against a limit of 2^24, so this is the
// path that always runs in practice; the clamped loop remains for the case the
// scan cannot rule out, and produces identical values when it does run.
//
// A port may skip this optimisation and always clamp. The results are
// identical either way; that is the whole argument for it being safe.
func (f *complexStage) beginPacket(steps int) {
	var maxAbs int64
	for _, w := range f.wr {
		if w < 0 {
			w = -w
		}
		if w > maxAbs {
			maxAbs = w
		}
	}
	for _, w := range f.wi {
		if w < 0 {
			w = -w
		}
		if w > maxAbs {
			maxAbs = w
		}
	}
	f.fast = maxAbs+2*f.mu*int64(steps) <= tapLimit
}

func (f *complexStage) push(xr, xi int64) {
	f.hr[f.idx], f.hi[f.idx] = xr, xi
	f.sr[f.idx], f.si[f.idx] = sgn(xr), sgn(xi)
	f.idx++
	if f.idx == len(f.hr) {
		n := f.order
		copy(f.hr, f.hr[f.idx-n:f.idx])
		copy(f.hi, f.hi[f.idx-n:f.idx])
		copy(f.sr, f.sr[f.idx-n:f.idx])
		copy(f.si, f.si[f.idx-n:f.idx])
		f.idx = n
	}
}

// forward is the encoder direction: return the residual for a known sample.
func (f *complexStage) forward(xr, xi int64) (int64, int64) {
	pr, pi := f.predict()
	er, ei := xr-pr, xi-pi
	f.adapt(er, ei)
	f.push(xr, xi)
	return er, ei
}

// inverse is the decoder direction: reconstruct a sample from its residual. It
// performs the same prediction, adaptation and history update as forward,
// which is what keeps the two sides identical.
func (f *complexStage) inverse(er, ei int64) (int64, int64) {
	pr, pi := f.predict()
	xr, xi := er+pr, ei+pi
	f.adapt(er, ei)
	f.push(xr, xi)
	return xr, xi
}

// realStage is complexStage with the imaginary terms removed, for mono audio.
type realStage struct {
	order int
	mu    int64
	w     []int64
	h     []int64
	s     []int64
	idx   int
	fast  bool
}

func newRealStage(order int, mu int64) *realStage {
	n := historyLen(order)
	return &realStage{
		order: order, mu: mu,
		w:   make([]int64, order),
		h:   make([]int64, n),
		s:   make([]int64, n),
		idx: order,
	}
}

func (f *realStage) predict() int64 {
	w := f.w
	h := f.h[f.idx-len(w) : f.idx]
	h = h[:len(w)]
	var p int64
	for j, wv := range w {
		p += wv * h[j]
	}
	return roundShift(p, tapShift)
}

// adapt is the real form of complexStage.adapt, leaking leakShiftReal off each
// tap alongside the gradient step. The early return on a zero error is required
// for the same reason: it is what freezes the taps over digital silence rather
// than leaking them towards zero.
func (f *realStage) adapt(e int64) {
	if e == 0 {
		return
	}
	m := f.mu * sgn(e)
	w := f.w
	s := f.s[f.idx-len(w) : f.idx]
	s = s[:len(w)]
	if f.fast {
		for j, sv := range s {
			w[j] += m*sv - leak(w[j], leakShiftReal)
		}
		return
	}
	for j, sv := range s {
		w[j] = clampTap(w[j] + m*sv - leak(w[j], leakShiftReal))
	}
}

// beginPacket is the real form: one update moves a tap by at most mu, so the
// bound is mu*steps.
func (f *realStage) beginPacket(steps int) {
	var maxAbs int64
	for _, w := range f.w {
		if w < 0 {
			w = -w
		}
		if w > maxAbs {
			maxAbs = w
		}
	}
	f.fast = maxAbs+f.mu*int64(steps) <= tapLimit
}

func (f *realStage) push(x int64) {
	f.h[f.idx], f.s[f.idx] = x, sgn(x)
	f.idx++
	if f.idx == len(f.h) {
		n := f.order
		copy(f.h, f.h[f.idx-n:f.idx])
		copy(f.s, f.s[f.idx-n:f.idx])
		f.idx = n
	}
}

func (f *realStage) forward(x int64) int64 {
	p := f.predict()
	e := x - p
	f.adapt(e)
	f.push(x)
	return e
}

func (f *realStage) inverse(e int64) int64 {
	p := f.predict()
	x := e + p
	f.adapt(e)
	f.push(x)
	return x
}
