// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Packet header.
//
// The obvious design is a fixed record on every packet: timestamp, sample
// rate, channel count, sample count, signal quality. That costs 37 bytes and
// almost all of it is a repeat. Sample rate and channel count change only on a
// mode change. Signal quality is measured at about 10 Hz, so at the 1099
// packets a second of a 384 kHz IQ stream the same pair is retransmitted a
// hundred times. Only the timestamp really changes, and it changes by a small
// amount.
//
// So the header carries a magic, a flags byte, and then only the fields that
// have actually changed since the last packet. Measured on live traffic this
// averages 7.7 bytes on IQ and 9.1 on audio, against a fixed 37.
//
//	[magic u32]                                   4      always
//	[flags u8]                                    1      always
//	[timestamp]                              8 or 1-9    see below
//	[sampleCount uvarint]                       1-9      if FlagCount
//	[sampleRate uvarint][channels u8]           2-10     if FlagMetadata
//	[power i16][noise i16]                        4      if FlagQuality
//
// Observed sizes: 8 bytes steady state, 23 bytes at a resynchronisation point
// for a 384 kHz two-channel stream, 25 bytes for the widest plausible rate and
// count.
//
// The header is a metadata record, not a container. It says nothing about how
// long the body is: on a WebSocket the frame boundary gives that, and in a
// file the container's length prefix does.
const (
	// Magic identifies a packet header. Little-endian on the wire it reads
	// "PCM4", which makes it findable in a hex dump; the string is retained
	// from the format's original name and carries no meaning of its own.
	//
	// Four bytes and not one or two because in the deployment this format
	// comes from, lossless packets share a socket with lossy Opus frames that
	// carry no magic of their own -- the sender chooses the format per packet.
	// The width of the magic is therefore a false-positive rate, and each
	// false positive corrupts one frame of audio:
	//
	//	1 byte   1 in 256      about 4 bad frames a second at 1099 packets/s
	//	2 bytes  1 in 65536    one every 59 seconds
	//	4 bytes  1 in 2^32     one every few years
	//
	// Two bytes looks sufficient until the rate is worked out.
	Magic uint32 = 0x344D4350

	// FlagEscape marks a body holding verbatim little-endian int16 samples
	// rather than a coded bitstream.
	FlagEscape = 1 << 7

	// FlagQuality marks a header carrying the signal-quality pair.
	FlagQuality = 1 << 6

	// FlagMetadata marks a resynchronisation point: the header carries the
	// sample rate and channel count, and its timestamp is a full 64-bit value
	// rather than a delta. The two always coincide -- both mean "this packet
	// does not depend on the one before it" -- so one bit marks both.
	FlagMetadata = 1 << 5

	// FlagSilent marks a packet whose samples are all zero. No body is
	// transmitted at all.
	FlagSilent = 1 << 4

	// FlagCount marks a header carrying the sample count.
	FlagCount = 1 << 3

	// ProfileMask extracts the payload codec profile from the low three bits,
	// so eight profiles. Two are defined; see profiles.go.
	ProfileMask = 0x07
)

const (
	// QualityNoReading is the codepoint for "the receiver reported nothing".
	// It stands in for the -999 sentinel used at the API level, which cannot
	// be represented in centidecibels because -99900 overflows an int16.
	QualityNoReading int16 = -32768

	qualityMinDB = -327.67
	qualityMaxDB = 327.67

	// DefaultResyncNanos is how often metadata is retransmitted regardless of
	// change. Five seconds is short enough that a reader opening a stored
	// stream at an arbitrary offset waits a negligible time to learn the
	// sample rate, and long enough that the cost rounds to about 0.02 bytes
	// per packet.
	//
	// It restores the HEADER only. Predictor state is never retransmitted, so
	// this is not a keyframe -- see SPEC.md, "Random access".
	DefaultResyncNanos uint64 = 5_000_000_000
)

// QualityFromFloat converts a dB reading to the signed centidecibels the
// header carries.
//
// 0.01 dB resolution keeps the error in a derived SNR under 0.01 dB, which is
// far below anything a receiver UI displays. Two bytes rather than four costs
// nothing worth measuring because the pair is only sent when it changes;
// dropping to one byte would put half a dB into the SNR, which a one-decimal
// readout would show.
//
// The -999 sentinel, and any non-finite value, become QualityNoReading. NaN
// takes that path because a NaN comparison is false, which is intended: a
// reading that is not a number is not a reading. Finite values outside the
// representable range are clamped rather than second-guessed.
func QualityFromFloat(v float32) int16 {
	d := float64(v)
	if !(d > -998) { // also catches NaN and -Inf
		return QualityNoReading
	}
	if d < qualityMinDB {
		d = qualityMinDB
	}
	if d > qualityMaxDB {
		d = qualityMaxDB
	}
	return int16(math.Round(d * 100))
}

// QualityToFloat reverses QualityFromFloat, returning -999 for "no reading".
func QualityToFloat(q int16) float32 {
	if q == QualityNoReading {
		return -999
	}
	return float32(float64(q) / 100)
}

// Header is one packet's metadata in the terms callers use. Which fields were
// actually transmitted is the business of the header coder; every field here
// is filled in on every packet, carried forward from the last
// resynchronisation point when the packet did not repeat it.
type Header struct {
	// TimestampNanos is the time of the first sample in the packet,
	// nanoseconds since the Unix epoch. The format does not care what clock it
	// comes from; the encoder only requires that it advances monotonically
	// within a stream.
	TimestampNanos uint64

	// SampleRate in Hz, and Channels: 1 for demodulated audio, 2 for
	// interleaved I/Q.
	SampleRate int
	Channels   int

	// SampleCount is how many int16 samples the body holds, counting both
	// channels of an interleaved IQ frame. A coded body has no length-to-count
	// relationship -- that is what compression means -- so this is what tells
	// the decoder when to stop.
	//
	// It cannot be assumed constant: a real receiver does not deliver a fixed
	// packet size. Across the reference captures a 384 kHz stream carries 720
	// samples on 95% of packets and 240 on the rest.
	SampleCount int

	// BasebandPower and Noise are signal-quality readings in dBFS, or -999
	// when the receiver reported nothing. They are passengers: nothing in the
	// codec reads them. A stream that has no such readings sets both to -999
	// and pays four bytes once.
	BasebandPower float32
	Noise         float32

	// Profile is the payload codec profile the body was coded with. See
	// profiles.go: the packet declares it, and a decoder obeys the declaration
	// rather than inferring a predictor from the channel count.
	Profile byte

	// Escape reports that the body holds verbatim samples.
	Escape bool

	// Silent reports that every sample is zero and no body was transmitted.
	// Escape and Silent are mutually exclusive.
	Silent bool
}

// HeaderEncoder writes headers for one stream.
//
// Stateful: it tracks what the peer has already been told. Create one per
// stream and use it from a single goroutine.
type HeaderEncoder struct {
	// ResyncNanos is the periodic resynchronisation interval. Zero selects
	// DefaultResyncNanos. Set it before the first packet.
	ResyncNanos uint64

	started    bool
	lastTS     uint64
	lastRate   int
	lastCh     int
	lastPower  int16
	lastNoise  int16
	lastCount  int
	lastResync uint64
}

// NewHeaderEncoder returns an encoder that has told the peer nothing, so its
// first packet is a full resynchronisation point.
func NewHeaderEncoder() *HeaderEncoder {
	return &HeaderEncoder{ResyncNanos: DefaultResyncNanos}
}

func (e *HeaderEncoder) resyncInterval() uint64 {
	if e.ResyncNanos == 0 {
		return DefaultResyncNanos
	}
	return e.ResyncNanos
}

// needsResync decides whether this packet retransmits metadata and a full
// timestamp.
func (e *HeaderEncoder) needsResync(h Header) bool {
	if !e.started || h.SampleRate != e.lastRate || h.Channels != e.lastCh {
		return true
	}
	iv := e.resyncInterval()

	// A timestamp that moves backwards, or jumps further than the resync
	// interval, means the stream is not continuous where a delta would assume
	// it was. Resynchronise rather than encode a delta nobody can sanity-check.
	if h.TimestampNanos < e.lastTS || h.TimestampNanos-e.lastTS > iv {
		return true
	}
	return h.TimestampNanos >= e.lastResync && h.TimestampNanos-e.lastResync >= iv
}

// Append appends the wire header for h to dst and returns the extended slice.
func (e *HeaderEncoder) Append(dst []byte, h Header) []byte {
	power := QualityFromFloat(h.BasebandPower)
	noise := QualityFromFloat(h.Noise)
	resync := e.needsResync(h)

	flags := h.Profile & ProfileMask
	if h.Escape {
		flags |= FlagEscape
	}
	if h.Silent {
		flags |= FlagSilent
	}
	if resync {
		flags |= FlagMetadata
	}
	// Quality rides every resynchronisation so such a packet is completely
	// self-describing, and otherwise only when it has changed. On the
	// reference captures that is 1% of packets on a 384 kHz IQ stream and 20%
	// on audio.
	sendQuality := resync || power != e.lastPower || noise != e.lastNoise
	if sendQuality {
		flags |= FlagQuality
	}
	sendCount := resync || h.SampleCount != e.lastCount
	if sendCount {
		flags |= FlagCount
	}

	dst = binary.LittleEndian.AppendUint32(dst, Magic)
	dst = append(dst, flags)

	var scratch [binary.MaxVarintLen64]byte
	if resync {
		dst = binary.LittleEndian.AppendUint64(dst, h.TimestampNanos)
	} else {
		// Deltas are not uniform -- a receiver delivers in bursts, and gaps
		// measured on live traffic ranged from 2 microseconds to 28
		// milliseconds -- but a varint absorbs that, averaging 2.5 bytes on IQ
		// and 4 on audio, and is never worse than the 8 an absolute costs.
		n := binary.PutVarint(scratch[:], int64(h.TimestampNanos)-int64(e.lastTS))
		dst = append(dst, scratch[:n]...)
	}
	if sendCount {
		n := binary.PutUvarint(scratch[:], uint64(h.SampleCount))
		dst = append(dst, scratch[:n]...)
	}
	if resync {
		n := binary.PutUvarint(scratch[:], uint64(h.SampleRate))
		dst = append(dst, scratch[:n]...)
		dst = append(dst, byte(h.Channels))
	}
	if sendQuality {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(power))
		dst = binary.LittleEndian.AppendUint16(dst, uint16(noise))
	}

	e.started = true
	e.lastTS = h.TimestampNanos
	e.lastRate = h.SampleRate
	e.lastCh = h.Channels
	e.lastPower = power
	e.lastNoise = noise
	e.lastCount = h.SampleCount
	if resync {
		e.lastResync = h.TimestampNanos
	}
	return dst
}

// HeaderDecoder reads headers for one stream, carrying forward whatever the
// encoder chose not to repeat.
//
// Stateful, per stream, single goroutine -- like the encoder.
type HeaderDecoder struct {
	haveMetadata bool
	lastTS       uint64
	rate         int
	channels     int
	count        int
	power        int16
	noise        int16
}

// NewHeaderDecoder returns a decoder that has not yet seen metadata and so
// rejects packets until a resynchronisation point arrives.
func NewHeaderDecoder() *HeaderDecoder {
	return &HeaderDecoder{power: QualityNoReading, noise: QualityNoReading}
}

// Decode parses the header at the front of pkt, returning it and the offset at
// which the body begins.
//
// A packet arriving before any metadata has been seen is rejected rather than
// guessed at. That is the normal state for a reader that opened a stored
// stream at an arbitrary offset; the periodic resynchronisation ends it.
func (d *HeaderDecoder) Decode(pkt []byte) (Header, int, error) {
	var h Header
	if len(pkt) < 5 {
		return h, 0, fmt.Errorf("ubersdriq: header: packet too short (%d bytes)", len(pkt))
	}
	if magic := binary.LittleEndian.Uint32(pkt); magic != Magic {
		return h, 0, fmt.Errorf("ubersdriq: header: bad magic 0x%08x", magic)
	}
	flags := pkt[4]
	off := 5

	h.Profile = flags & ProfileMask
	h.Escape = flags&FlagEscape != 0
	h.Silent = flags&FlagSilent != 0
	if h.Escape && h.Silent {
		return h, 0, fmt.Errorf("ubersdriq: header: escape and silent are mutually exclusive")
	}

	if flags&FlagMetadata != 0 {
		if len(pkt) < off+8 {
			return h, 0, fmt.Errorf("ubersdriq: header: truncated timestamp")
		}
		d.lastTS = binary.LittleEndian.Uint64(pkt[off:])
		off += 8
	} else {
		if !d.haveMetadata {
			return h, 0, fmt.Errorf("ubersdriq: header: delta packet before any resynchronisation point")
		}
		delta, n := binary.Varint(pkt[off:])
		if n <= 0 {
			return h, 0, fmt.Errorf("ubersdriq: header: malformed timestamp delta")
		}
		off += n
		d.lastTS = uint64(int64(d.lastTS) + delta)
	}
	h.TimestampNanos = d.lastTS

	if flags&FlagCount != 0 {
		count, n := binary.Uvarint(pkt[off:])
		if n <= 0 {
			return h, 0, fmt.Errorf("ubersdriq: header: malformed sample count")
		}
		off += n
		d.count = int(count)
	}

	if flags&FlagMetadata != 0 {
		rate, n := binary.Uvarint(pkt[off:])
		if n <= 0 {
			return h, 0, fmt.Errorf("ubersdriq: header: malformed sample rate")
		}
		off += n
		if len(pkt) < off+1 {
			return h, 0, fmt.Errorf("ubersdriq: header: truncated channel count")
		}
		d.rate = int(rate)
		d.channels = int(pkt[off])
		off++
		d.haveMetadata = true
	} else if !d.haveMetadata {
		return h, 0, fmt.Errorf("ubersdriq: header: payload before any metadata")
	}

	if flags&FlagQuality != 0 {
		if len(pkt) < off+4 {
			return h, 0, fmt.Errorf("ubersdriq: header: truncated signal quality")
		}
		d.power = int16(binary.LittleEndian.Uint16(pkt[off:]))
		d.noise = int16(binary.LittleEndian.Uint16(pkt[off+2:]))
		off += 4
	}

	if d.rate <= 0 || d.channels <= 0 {
		return h, 0, fmt.Errorf("ubersdriq: header: implausible metadata (rate %d, channels %d)", d.rate, d.channels)
	}
	if d.count <= 0 {
		return h, 0, fmt.Errorf("ubersdriq: header: implausible sample count %d", d.count)
	}

	h.SampleRate = d.rate
	h.Channels = d.channels
	h.SampleCount = d.count
	h.BasebandPower = QualityToFloat(d.power)
	h.Noise = QualityToFloat(d.noise)
	return h, off, nil
}

// IsPacket reports whether a binary frame carries a UberSDR-IQ header. Useful
// where these share a transport with frames of another kind.
func IsPacket(pkt []byte) bool {
	return len(pkt) >= 4 && binary.LittleEndian.Uint32(pkt) == Magic
}
