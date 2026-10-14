// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

//go:build amd64 && !purego

package ubersdriq

// simdState is the order-16 complex stage as the SIMD kernel holds it: taps
// and the newest 16 history samples, oldest first, in int32. block_amd64.s
// reads it at fixed offsets.
//
// int32 is exact: a tap never leaves +-tapLimit (2^24) -- the kernel always
// clamps, which where the Go code skips the clamp is an identity -- and the
// products are formed in int64 lanes. A history sample outside int32 can only
// come from a corrupt stream; the kernel is not used while one is in the
// window, and the decoder kernel stops before taking one in.
type simdState struct {
	wr, wi [16]int32
	hr, hi [16]int32
}

// load fills st from f, reporting false if the history does not fit int32.
func (st *simdState) load(f *complexStage) bool {
	lo := f.idx - 16
	for j := 0; j < 16; j++ {
		r, i := f.hr[lo+j], f.hi[lo+j]
		if r != int64(int32(r)) || i != int64(int32(i)) {
			return false
		}
		st.wr[j], st.wi[j] = int32(f.wr[j]), int32(f.wi[j])
		st.hr[j], st.hi[j] = int32(r), int32(i)
	}
	return true
}

// store writes st back to f, the window at the front of the history.
func (st *simdState) store(f *complexStage) {
	for j := 0; j < 16; j++ {
		f.wr[j], f.wi[j] = int64(st.wr[j]), int64(st.wi[j])
		r, i := int64(st.hr[j]), int64(st.hi[j])
		f.hr[j], f.hi[j] = r, i
		f.sr[j], f.si[j] = sgn(r), sgn(i)
	}
	f.idx = 16
}

// forwardSIMD runs forwardBlock16 through the kernel, reporting false when it
// cannot.
func (f *complexStage) forwardSIMD(samples []int16, res []int32) bool {
	var st simdState
	if !useSIMD || len(samples) < 2 || !st.load(f) {
		return false
	}
	simdForward(&st, &samples[0], &res[0], len(samples)/2, f.mu)
	st.store(f)
	return true
}

// inverseSIMD runs inverseBlock16 through the kernel, returning how many
// samples it reconstructed; the caller does the rest frame by frame.
func (f *complexStage) inverseSIMD(res []int32, out []int16) int {
	var st simdState
	if !useSIMD || len(res) < 2 || !st.load(f) {
		return 0
	}
	frames := simdInverse(&st, &res[0], &out[0], len(res)/2, f.mu)
	st.store(f)
	return 2 * frames
}
