// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

import "fmt"

// Packet assembly: the header and the payload codec together.
//
// A StreamEncoder holds the adaptation state of its predictor and the record
// of what the peer has already been told, so it belongs to exactly one stream
// and one goroutine.

// StreamEncoder builds packets for one stream.
type StreamEncoder struct {
	header *HeaderEncoder
	codec  *Codec

	// profile is the codec configuration in use. When the channel count
	// changes the predictor form must change with it, so the codec is rebuilt.
	profile byte

	packet []byte

	// scratch holds the requantised copy EncodeScaled codes, so the caller's
	// samples are never modified.
	scratch []int16
}

// NewStreamEncoder returns an encoder that has told the peer nothing yet.
func NewStreamEncoder() *StreamEncoder {
	return &StreamEncoder{
		header:  NewHeaderEncoder(),
		profile: 0xff, // no codec yet; the first packet builds one
	}
}

// HeaderEncoder exposes the header coder, so a caller can set ResyncNanos
// before the first packet.
func (e *StreamEncoder) HeaderEncoder() *HeaderEncoder { return e.header }

// Profile reports the predictor in use, or 0xff before the first packet.
func (e *StreamEncoder) Profile() byte { return e.profile }

// Encode assembles one lossless packet.
//
// samples are int16 values, interleaved I/Q when channels is 2.
// timestampNanos is the time of the first sample. basebandPower and noise are
// signal-quality readings in dBFS, or -999 for "no reading"; a stream that
// carries no such readings passes -999 for both.
//
// The returned slice is reused by the next call, so it must be written out or
// copied before this is called again.
func (e *StreamEncoder) Encode(
	samples []int16,
	timestampNanos uint64,
	sampleRate int,
	channels int,
	basebandPower float32,
	noise float32,
) ([]byte, error) {
	return e.encode(samples, timestampNanos, sampleRate, channels, basebandPower, noise, false, 0)
}

// EncodeScaled assembles one reduced-depth IQ packet: ProfileIQScaled, with
// samples requantised by shift before the predictor sees them and the shift
// itself as the first byte of the body.
//
// shift must be 0..MaxShift, and channels must be 2 -- there is no scaled real
// profile. The caller's samples are not modified; the requantisation is done on
// a copy, because it is lossy and the caller may want the originals.
//
// Which shift to use is the caller's policy and nothing on the wire depends on
// how it was chosen; see scaled.go. A shift of 0 is legal and codes the samples
// exactly, but a sender that wants a lossless stream should use Encode and
// profile 0 rather than declaring a lossy profile and shifting by nothing --
// that keeps an archival stream honestly labelled.
func (e *StreamEncoder) EncodeScaled(
	samples []int16,
	timestampNanos uint64,
	sampleRate int,
	channels int,
	basebandPower float32,
	noise float32,
	shift uint,
) ([]byte, error) {
	if channels != 2 {
		return nil, fmt.Errorf("ubersdriq: the scaled profile is complex, so it needs 2 channels, not %d", channels)
	}
	if shift > MaxShift {
		return nil, fmt.Errorf("ubersdriq: shift %d out of range 0..%d", shift, MaxShift)
	}
	e.scratch = append(e.scratch[:0], samples...)
	Quantise(e.scratch, shift)
	return e.encode(e.scratch, timestampNanos, sampleRate, channels, basebandPower, noise, true, shift)
}

func (e *StreamEncoder) encode(
	samples []int16,
	timestampNanos uint64,
	sampleRate int,
	channels int,
	basebandPower float32,
	noise float32,
	scaled bool,
	shift uint,
) ([]byte, error) {
	if channels <= 0 {
		channels = 1
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("ubersdriq: empty packet")
	}

	// The predictor form follows the channel count, so a mode change that
	// alters it rebuilds the codec. Discarding the adaptation is correct
	// rather than merely acceptable: the new mode is a different signal, and
	// taps trained on the old one would predict it worse than a cold start.
	// The header notices the same change independently and retransmits
	// metadata, so the two stay consistent without talking to each other.
	//
	// Crossing between the lossless and the scaled IQ profile is the same
	// event: the profile id on the wire changes, so both ends rebuild.
	want := ProfileForChannels(channels)
	if scaled {
		want = ProfileIQScaled
	}
	if e.codec == nil || want != e.profile {
		codec, err := NewCodec(want)
		if err != nil {
			return nil, err
		}
		e.codec = codec
		e.profile = want
	}

	// A complex profile consumes whole I/Q frames. A truncated final frame
	// would desynchronise the predictor against the decoder, so drop it rather
	// than code a half sample.
	if e.codec.SamplesPerStep() == 2 && len(samples)%2 != 0 {
		samples = samples[:len(samples)-1]
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("ubersdriq: no whole frames in packet")
	}

	// Detecting silence rather than being told about it catches every source
	// of it -- a closed squelch, a muted session, a dead channel -- and costs
	// almost nothing: measured at 0.18 us on a 480-byte packet and 0.52 us on
	// 1440, against roughly 30 us to encode one. The scan is no more expensive
	// when the first non-zero sample is the last one, so no input makes it
	// pathological.
	silent := true
	for _, v := range samples {
		if v != 0 {
			silent = false
			break
		}
	}

	var body []byte
	var escape bool
	if silent {
		if err := e.codec.AdvanceSilence(len(samples)); err != nil {
			return nil, err
		}
	} else {
		var err error
		body, escape, err = e.codec.Encode(samples)
		if err != nil {
			return nil, err
		}
	}

	e.packet = e.header.Append(e.packet[:0], Header{
		TimestampNanos: timestampNanos,
		SampleRate:     sampleRate,
		Channels:       channels,
		SampleCount:    len(samples),
		BasebandPower:  basebandPower,
		Noise:          noise,
		Profile:        want,
		Escape:         escape,
		Silent:         silent,
	})
	// The shift leads the body rather than riding in the header: the flags
	// byte is full, and a silent packet has no body at all, so putting it here
	// costs nothing on a squelched or dead channel.
	if scaled && !silent {
		e.packet = append(e.packet, byte(shift))
	}
	e.packet = append(e.packet, body...)
	return e.packet, nil
}

// StreamDecoder is the receiving half.
type StreamDecoder struct {
	header  *HeaderDecoder
	codec   *Codec
	profile byte
}

// NewStreamDecoder returns a decoder with no state, which rejects packets
// until a resynchronisation point arrives.
func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{header: NewHeaderDecoder(), profile: 0xff}
}

// Decode reverses Encode, returning the header and the samples as values,
// interleaved I/Q when the header reports two channels.
//
// The packet is self-contained: the header carries the sample count, so
// nothing has to be told out of band how long the body is.
//
// Every packet of a stream must pass through here, in order, starting from the
// first. A packet that is skipped -- even one whose samples the caller intends
// to discard -- leaves this side's filters where the encoder's no longer are,
// and every packet after it decodes to noise.
func (d *StreamDecoder) Decode(pkt []byte) (Header, []int16, error) {
	h, off, err := d.header.Decode(pkt)
	if err != nil {
		return h, nil, err
	}

	// The packet declares its own profile; nothing here infers it from the
	// channel count. A profile this build does not implement is an error
	// rather than a fallback.
	if d.codec == nil || h.Profile != d.profile {
		codec, err := NewCodec(h.Profile)
		if err != nil {
			return h, nil, err
		}
		d.codec = codec
		d.profile = h.Profile
	}

	if h.Silent {
		if len(pkt) != off {
			return h, nil, fmt.Errorf("ubersdriq: silent packet carries %d bytes of body", len(pkt)-off)
		}
		if err := d.codec.AdvanceSilence(h.SampleCount); err != nil {
			return h, nil, err
		}
		return h, make([]int16, h.SampleCount), nil
	}

	// A scaled profile carries the shift as the first byte of the body. It is
	// read only here, after the silent case has returned: a silent packet on a
	// scaled profile carries no shift, because it carries no body.
	var shift uint
	if d.codec.Profile().Scaled {
		if len(pkt) <= off {
			return h, nil, fmt.Errorf("ubersdriq: scaled packet carries no shift byte")
		}
		shift = uint(pkt[off])
		if shift > MaxShift {
			return h, nil, fmt.Errorf("ubersdriq: shift %d out of range 0..%d", shift, MaxShift)
		}
		off++
	}

	samples, err := d.codec.Decode(pkt[off:], h.SampleCount, h.Escape)
	if err != nil {
		return h, nil, err
	}
	// Undo the scale only on the way out. The predictor above ran on the
	// values that were on the wire, exactly as the encoder's did.
	Restore(samples, shift)
	return h, samples, nil
}
