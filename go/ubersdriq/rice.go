// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// Rice coding of residuals.
//
// A residual is coded as its zigzagged magnitude split at bit k: the high part
// in unary, then a 0 stop bit, then the low k bits raw. k is chosen once per
// packet from the mean magnitude, which for a Laplacian source is within a
// fraction of a bit of optimal, and transmitted as the first byte of the body.
//
// Prediction residuals from this kind of signal really are close to Laplacian,
// and the measurements say so. Against the order-0 entropy of the residuals
// themselves, computed over each whole reference capture, the Rice bitstream
// costs between 0.975 and 1.031 times as much -- within about 3%, and
// occasionally under 1.0 because k adapts per packet where the entropy figure
// is one global distribution.
//
// The alternatives were measured and rejected. A full adaptive
// arithmetic/rANS model over the residual alphabet came out level (1.35-1.39x
// either way) at far greater cost. Adapting k continuously rather than per
// packet was slightly worse. FLAC-style partitioning changed nothing. Unary
// accounts for only 16-24% of the coded bits, so there is no escape-coding win
// hiding either.
//
// Bit order is least-significant-bit first within each byte, and bytes are
// written in increasing address order. The encoder below buffers into a 64-bit
// accumulator and flushes 32 bits at a time as a little-endian uint32, which
// is the same byte sequence as writing one byte at a time.

// zigzag folds a signed value onto the non-negative integers so small
// magnitudes of either sign get short codes.
func zigzag(v int32) uint32 {
	return uint32((v << 1) ^ (v >> 31))
}

// riceScratchLen sizes the encoder's working buffer.
//
// A coded body cannot usefully exceed the 2 bytes per sample the escape costs,
// because the escape is taken when it would -- but the encoder writes the
// coded form first and so must have room for a pathological bitstream before
// it can measure one.
func riceScratchLen(n int) int { return n*5 + 64 }

// riceEncode appends the k byte and the bitstream for res to dst.
//
// dst must have capacity for riceScratchLen(len(res)) beyond its current
// length; the caller sizes it.
func riceEncode(res []int32, dst []byte) []byte {
	var sum uint64
	for _, v := range res {
		sum += uint64(zigzag(v))
	}
	k := uint(0)
	if m := sum / uint64(len(res)); m > 0 {
		k = uint(bits.Len64(m)) - 1
		if k > 30 {
			k = 30
		}
	}

	start := len(dst)
	dst = append(dst, byte(k))
	buf := dst[start+1 : cap(dst)]
	buf = buf[:cap(buf)]

	// The bit accumulator lives in locals rather than behind a closure: a
	// closure containing a loop cannot inline, and the call per codeword was
	// measurable. The accumulator is flushed in 32-bit units, so nbits is
	// always below 32 at the top of each iteration and anything appended must
	// be at most 32 bits wide for its shift to stay inside a uint64.
	var acc uint64
	var nbits uint
	i := 0
	mask := uint32(1)<<k - 1

	for _, v := range res {
		u := zigzag(v)
		q := uint(u >> k)

		// The whole codeword fits one write when it is short enough. The bound
		// is 24 and not something larger because nbits can already be 31: at
		// 24 the shift needs 55 bits, safely inside a uint64, whereas allowing
		// 40 here would need 71 and silently drop the top bits. That was a
		// real bug, and it only appeared on the large unpredicted samples of a
		// high-dynamic-range band, where the unary run gets long.
		if q+k+1 <= 24 {
			acc |= ((uint64(u&mask)<<1)<<q | (uint64(1)<<q - 1)) << nbits
			nbits += q + k + 1
		} else {
			// Long unary run: emit it in chunks, then the stop bit and
			// remainder.
			for r := q; r > 0; {
				c := r
				if c > 24 {
					c = 24
				}
				acc |= (uint64(1)<<c - 1) << nbits
				nbits += c
				for nbits >= 32 {
					binary.LittleEndian.PutUint32(buf[i:], uint32(acc))
					i += 4
					acc >>= 32
					nbits -= 32
				}
				r -= c
			}
			acc |= (uint64(u&mask) << 1) << nbits
			nbits += k + 1
		}
		for nbits >= 32 {
			binary.LittleEndian.PutUint32(buf[i:], uint32(acc))
			i += 4
			acc >>= 32
			nbits -= 32
		}
	}

	for nbits > 0 {
		buf[i] = byte(acc)
		i++
		acc >>= 8
		if nbits < 8 {
			break
		}
		nbits -= 8
	}
	return dst[:start+1+i]
}

// riceDecode reverses riceEncode into out, which must have length count.
func riceDecode(src []byte, out []int32) error {
	if len(src) < 1 {
		return fmt.Errorf("ubersdriq: rice: empty bitstream")
	}
	k := uint(src[0])
	if k > 30 {
		return fmt.Errorf("ubersdriq: rice: invalid k %d", k)
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
	mask := uint64(1)<<k - 1

	for j := range out {
		if nbits < 48 {
			refill()
		}
		// Count the run of 1 bits. Bits past nbits read as 0, so a run that
		// reaches the end of the accumulator is continued after a refill.
		var q uint
		for {
			c := uint(bits.TrailingZeros64(^acc))
			if c < nbits {
				q += c
				acc >>= c + 1
				nbits -= c + 1
				break
			}
			if i >= len(src) {
				return fmt.Errorf("ubersdriq: rice: truncated at value %d", j)
			}
			q += nbits
			acc, nbits = 0, 0
			refill()
		}
		if nbits < k {
			refill()
		}
		if nbits < k {
			return fmt.Errorf("ubersdriq: rice: truncated remainder at value %d", j)
		}
		u := uint32(q)<<k | uint32(acc&mask)
		acc >>= k
		nbits -= k
		out[j] = int32(u>>1) ^ -int32(u&1)
	}
	return nil
}
