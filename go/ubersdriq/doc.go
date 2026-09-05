// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// Package ubersdriq implements UberSDR-IQ, a lossless compression format for
// software-defined radio sample streams: complex baseband IQ and demodulated
// audio, both as 16-bit integers.
//
// The format exists because general-purpose compressors do not work on this
// data. An LZ77 matcher such as zstd or deflate looks for repeated byte
// strings; a band-limited RF signal has none, so the matcher correctly gives
// up and emits a stored block, paying a frame header for nothing. Measured on
// live receiver traffic, zstd returned 0.985 to 0.990 on IQ and 0.970 on
// demodulated audio -- output consistently larger than the input.
//
// What the samples do have is correlation between neighbours: |r(1)| from 0.03
// on an empty wideband segment up to 0.99 on narrowband AM. UberSDR-IQ
// predicts each sample from the ones before it with an adaptive filter and
// Rice-codes the prediction error, which turns that correlation into 1.4x to
// 4.5x on the same captures.
//
// The predictor is backward adaptive: its taps are derived from samples that
// have already been coded, so the decoder recomputes them independently and no
// coefficients are transmitted. Everything is integer arithmetic with defined
// rounding, so encoder and decoder agree bit for bit on any platform. That is
// what makes the lossless claim meaningful across implementations in different
// languages.
//
// # Layers
//
// Three layers, each usable on its own:
//
//	Codec           the payload codec: samples in, coded body out
//	HeaderEncoder   the packet header: a delta-coded metadata record
//	StreamEncoder   the two together, producing whole packets
//
// Most callers want StreamEncoder and StreamDecoder.
//
// # Lifetime
//
// A Codec, a header coder and a Stream* instance are each one stream. They
// carry the adaptation of every sample seen so far and the record of what the
// peer has already been told, so each belongs to exactly one connection or
// file, is used from a single goroutine, and is discarded when that ends.
// None of them is safe for concurrent use.
//
// A decoder must see every packet in order, starting from the first. There is
// no keyframe: predictor state is never retransmitted, so a decoder that joins
// part-way through a stream produces wrong samples indefinitely. See SPEC.md,
// "Random access".
//
// # Reduced-depth IQ
//
// One profile, ProfileIQScaled, is the exception to "lossless": it codes IQ
// samples that have been requantised by a power of two, for a client that has
// asked for a reduced depth, and carries that shift in the packet. The codec is
// still exact over what it is given, and a stream that never declares that
// profile is lossless end to end. EncodeScaled produces it; the decoder handles
// it without being asked. See scaled.go and SPEC.md section 6.6.
package ubersdriq
