// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

// SetSIMD turns the SIMD kernel on or off for the external tests, returning
// a function that restores it.
func SetSIMD(on bool) (restore func()) {
	saved := useSIMD
	useSIMD = on
	return func() { useSIMD = saved }
}
