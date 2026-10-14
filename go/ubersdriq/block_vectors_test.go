// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq_test

import (
	"testing"

	"github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

// TestVectorsPureGoBlock reruns the conformance vectors with the SIMD kernel
// off, so the pure-Go block is held to them on every machine.
func TestVectorsPureGoBlock(t *testing.T) {
	defer ubersdriq.SetSIMD(false)()
	t.Run("decode", TestVectorsDecodeExactly)
	t.Run("encode", TestVectorsEncodeByteForByte)
	t.Run("edge", TestEdgeCaseVector)
	t.Run("scaled", TestScaledIQVector)
	t.Run("silence", TestSilenceAdvancesIdentically)
}
