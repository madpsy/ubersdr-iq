// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

import (
	"encoding/binary"
	"fmt"
)

// Codec codes one direction of one stream's payloads.
//
// Stateful across packets and NOT safe for concurrent use: create one per
// stream, call it from a single goroutine, and drop it when the stream ends.
// Its taps carry the adaptation of every sample coded so far, which is exactly
// why it cannot be pooled or shared.
//
// Nothing resets it mid-stream. Packets dropped before they reach the encoder
// are not a problem -- both ends then see the same sequence and re-adapt over
// the gap -- but a packet dropped between encoder and decoder is fatal to
// everything after it, because the two would no longer be predicting from the
// same history. The format therefore assumes an ordered, lossless transport.
type Codec struct {
	prof Profile
	cx   []*complexStage
	rl   []*realStage

	res []int32
	buf []byte
}

// NewCodec builds a codec for the given profile id, rejecting one it does not
// implement.
//
// The error is deliberate. Falling back to a default profile would decode a
// stream with the wrong predictor and return plausible-looking noise rather
// than failing, which is the worst possible behaviour for a codec whose entire
// promise is bit-exactness.
func NewCodec(profileID byte) (*Codec, error) {
	p, err := LookupProfile(profileID)
	if err != nil {
		return nil, err
	}
	c := &Codec{prof: p}
	for i := range p.Orders {
		if p.Complex {
			c.cx = append(c.cx, newComplexStage(p.Orders[i], p.Mus[i]))
		} else {
			c.rl = append(c.rl, newRealStage(p.Orders[i], p.Mus[i]))
		}
	}
	return c, nil
}

// Profile reports the configuration in use.
func (c *Codec) Profile() Profile { return c.prof }

// SamplesPerStep is 2 for a complex profile, which consumes interleaved I/Q
// frames, and 1 for a real one.
func (c *Codec) SamplesPerStep() int {
	if c.prof.Complex {
		return 2
	}
	return 1
}

// beginPacket lets every stage decide once, from where its taps stand, whether
// this packet's adapt calls may skip the clamp. steps is how many times each
// stage will adapt: sample count for a real cascade, frame count for a complex
// one.
func (c *Codec) beginPacket(steps int) {
	for _, s := range c.cx {
		s.beginPacket(steps)
	}
	for _, s := range c.rl {
		s.beginPacket(steps)
	}
}

// forward runs the cascade in the encoder direction over one sample position.
// Each stage predicts the residual left by the one before it.
func (c *Codec) forward(a, b int64) (int64, int64) {
	if c.prof.Complex {
		for _, s := range c.cx {
			a, b = s.forward(a, b)
		}
		return a, b
	}
	for _, s := range c.rl {
		a = s.forward(a)
	}
	return a, 0
}

// Encode codes one packet and returns its body.
//
// For a complex profile, samples are interleaved I/Q and len(samples) must be
// even. escape reports that the body holds verbatim little-endian int16
// samples rather than a coded bitstream; the caller must carry that bit, and
// the profile id, in the packet header.
//
// The returned slice aliases an internal buffer that the next Encode call
// reuses. Copy it if it must outlive that.
func (c *Codec) Encode(samples []int16) (body []byte, escape bool, err error) {
	n := len(samples)
	step := c.SamplesPerStep()
	if n == 0 {
		return nil, false, fmt.Errorf("ubersdriq: empty packet")
	}
	if n%step != 0 {
		return nil, false, fmt.Errorf("ubersdriq: %d samples is not a whole number of %d-channel frames", n, step)
	}

	// Sized independently: Decode grows only res, so a codec that decoded
	// before it encoded -- which happens when the silent path advances the
	// predictor over a packet with no body -- would otherwise reach a nil buf.
	if cap(c.res) < n {
		c.res = make([]int32, n)
	}
	if cap(c.buf) < riceScratchLen(n) {
		c.buf = make([]byte, riceScratchLen(n))
	}
	c.res = c.res[:n]

	c.beginPacket(n / step)
	for i := 0; i < n; i += step {
		a := int64(samples[i])
		var b int64
		if step == 2 {
			b = int64(samples[i+1])
		}
		ra, rb := c.forward(a, b)
		c.res[i] = int32(ra)
		if step == 2 {
			c.res[i+1] = int32(rb)
		}
	}

	out := riceEncode(c.res, c.buf[:0])

	// If prediction and coding did not pay for themselves, send the samples as
	// they are.
	//
	// A predictor cannot help a full-entropy signal, and a saturated front end
	// produces exactly that. Without the escape such a stream would expand by
	// about 3%; with it the body never exceeds the samples, so the only
	// expansion left in the format is the header. Synthetic full-scale white
	// noise takes this path on every packet; none of the nine live captures in
	// the reference set takes it even once.
	//
	// The filters have already adapted over this packet above, and the decoder
	// adapts over the same verbatim samples, so state stays in step through an
	// escape.
	if len(out) >= n*2 {
		out = c.buf[:n*2]
		for i, v := range samples {
			binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
		}
		return out, true, nil
	}
	return out, false, nil
}

// AdvanceSilence advances the filters over count zero-valued samples without
// producing a body.
//
// A packet whose samples are all zero says so in its header and carries no
// body at all. Rice coding cannot get an all-zero residual below one bit per
// sample -- it is a bare stop bit -- so a 240-sample audio packet costs 30
// bytes of stop bits however well the predictor does. That is structural, not
// waste, and the only way past it is to not send the body.
//
// The predictor still has to run. The decoder advances its own filters over
// the same zeros, and the two must agree sample for sample or everything after
// this packet decodes wrongly. This is exactly what Encode would do over a
// zero buffer: Rice coding never touches the filters, and the escape decision
// reads only the coded length.
//
// With sign-sign adaptation a zero history leaves the taps frozen rather than
// decaying, so whatever the filter had learned before the silence began is
// still there when signal returns.
func (c *Codec) AdvanceSilence(count int) error {
	step := c.SamplesPerStep()
	if count <= 0 {
		return fmt.Errorf("ubersdriq: empty packet")
	}
	if count%step != 0 {
		return fmt.Errorf("ubersdriq: %d samples is not a whole number of %d-channel frames", count, step)
	}
	c.beginPacket(count / step)
	for i := 0; i < count; i += step {
		c.forward(0, 0)
	}
	return nil
}

// Decode reconstructs the samples of one packet body.
//
// count is the number of int16 samples the packet carries, from the header;
// escape is the header's escape bit. The body must have been coded with this
// codec's profile -- check the header's profile id and rebuild if it changed.
func (c *Codec) Decode(body []byte, count int, escape bool) ([]int16, error) {
	step := c.SamplesPerStep()
	if count <= 0 || count%step != 0 {
		return nil, fmt.Errorf("ubersdriq: bad sample count %d for %d-channel profile", count, step)
	}

	out := make([]int16, count)

	if escape {
		if len(body) < count*2 {
			return nil, fmt.Errorf("ubersdriq: escape body truncated (%d bytes for %d samples)", len(body), count)
		}
		for i := 0; i < count; i++ {
			out[i] = int16(binary.LittleEndian.Uint16(body[2*i:]))
		}
		// Advance the filters over these samples exactly as the encoder did,
		// discarding the residuals it produced.
		c.beginPacket(count / step)
		for i := 0; i < count; i += step {
			a := int64(out[i])
			var b int64
			if step == 2 {
				b = int64(out[i+1])
			}
			c.forward(a, b)
		}
		return out, nil
	}

	if cap(c.res) < count {
		c.res = make([]int32, count)
	}
	c.res = c.res[:count]
	if err := riceDecode(body, c.res); err != nil {
		return nil, err
	}

	c.beginPacket(count / step)
	if c.prof.Complex {
		for i := 0; i < count; i += 2 {
			a, b := int64(c.res[i]), int64(c.res[i+1])
			// Stages are inverted in reverse order: the last stage to have
			// predicted is the first to be undone.
			for j := len(c.cx) - 1; j >= 0; j-- {
				a, b = c.cx[j].inverse(a, b)
			}
			out[i], out[i+1] = int16(a), int16(b)
		}
		return out, nil
	}
	for i := 0; i < count; i++ {
		a := int64(c.res[i])
		for j := len(c.rl) - 1; j >= 0; j-- {
			a = c.rl[j].inverse(a)
		}
		out[i] = int16(a)
	}
	return out, nil
}
