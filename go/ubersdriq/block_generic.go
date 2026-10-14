// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

//go:build !amd64 || purego

package ubersdriq

// No SIMD kernel on this platform; the block paths run in Go.
var useSIMD = false

func (f *complexStage) forwardSIMD(samples []int16, res []int32) bool { return false }

func (f *complexStage) inverseSIMD(res []int32, out []int16) int { return 0 }
