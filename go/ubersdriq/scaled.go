// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

// The scaled IQ profile.
//
// ProfileIQScaled requantises each packet of interleaved I/Q by a power of two
// before the predictor sees it, and undoes the scale after the decoder's
// predictor has produced its samples. The codec in between is untouched: the
// saving comes entirely from the numbers going into it being smaller, which
// shortens every Rice codeword.
//
// The scale is a shift, it is chosen per packet, and it travels in the packet,
// so both ends apply identical integer shifts and the predictor stays bit-exact
// on the quantised grid. Applying the shift on the way in and taking it off on
// the way out -- rather than shifting before the predictor on one side and
// after it on the other -- is what keeps the two sides in step. A decoder that
// restored before running the filters would diverge on its first packet.
//
// Choosing the shift is a sender-side policy question and is deliberately not
// specified. Nothing a decoder does depends on reproducing it: only the shift
// is on the wire, and a shift is not a policy. The reference sender works it
// out from a margin the client asks for -- how far below the band's own noise
// floor the quantisation floor must stay -- and re-derives it per packet from
// the packet's crest factor and the band's spectral peakiness, because a depth
// that is transparent on medium wave wastes three quarters of the available
// saving on a quiet band.

// Quantise applies a shift to samples in place, rounding to nearest and
// clamping to the depth the shift implies. It is the sender's half of
// ProfileIQScaled; a decoder never performs it.
//
// Rounding rather than truncating matters: at ten bits half a step of the
// coarse grid is not a rounding detail, it is 42 dB below the peak. Clamping
// matters because rounding can carry a full-scale peak one step past the
// ceiling, and restoring that would wrap a full-scale positive sample to
// full-scale negative.
func Quantise(samples []int16, shift uint) {
	if shift == 0 {
		return
	}
	half := int32(1) << (shift - 1)
	lo, hi := int32(-32768)>>shift, int32(32767)>>shift
	for i, v := range samples {
		q := (int32(v) + half) >> shift
		if q < lo {
			q = lo
		} else if q > hi {
			q = hi
		}
		samples[i] = int16(q)
	}
}

// Restore is the decoder's half: the samples back onto the int16 grid.
//
// The saturation is not decoration. A decoder that has just reconstructed a
// value at the top of the quantised range shifts it to exactly 32767 or
// -32768; one that shifted without saturating could produce a value outside
// int16 and wrap it to the opposite rail, turning a strong signal into a loud
// artefact. Both ends must agree on the saturation, so it is specified.
func Restore(samples []int16, shift uint) {
	if shift == 0 {
		return
	}
	for i, v := range samples {
		r := int32(v) << shift
		if r > 32767 {
			r = 32767
		} else if r < -32768 {
			r = -32768
		}
		samples[i] = int16(r)
	}
}
