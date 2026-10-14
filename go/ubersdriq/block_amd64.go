// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

//go:build amd64 && !purego

package ubersdriq

// useSIMD selects the AVX2 kernel. A variable so the tests can hold the
// pure-Go block to the same output.
var useSIMD = hasAVX2()

// hasAVX2 reports AVX2 support, OS state saving included.
func hasAVX2() bool

// simdForward is forwardBlock16 over frames I/Q frames.
//
//go:noescape
func simdForward(st *simdState, samples *int16, res *int32, frames int, mu int64)

// simdInverse is inverseBlock16 over up to frames I/Q frames, returning how
// many it did: it stops before a reconstructed sample that leaves int32.
//
//go:noescape
func simdInverse(st *simdState, res *int32, out *int16, frames int, mu int64) int
