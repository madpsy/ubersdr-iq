// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

import "fmt"

// Predictor profiles.
//
// A profile is data, not code: both filter forms below are the same leaky
// sign-sign LMS algorithm, the real one being the complex one with the
// imaginary terms dropped, so a profile only chooses which form to instantiate,
// with what stage shapes, and whether the body carries a scale.
//
// # Why there are two filter forms
//
// The difference between them is forced by the signal rather than chosen.
//
// Complex baseband IQ: a carrier in it is a single complex pole, which one
// complex tap cancels exactly. Treating I and Q as two independent real
// streams throws that away. Measured on a medium-wave capture, the real form
// reached 1.29x where the complex form reached 1.89x.
//
// Demodulated audio: mono, with no quadrature partner and no frequency offset
// to rotate out, so the filter is the ordinary real one.
//
// The stage COUNT differs too, and only measurement settles it. Audio gains
// enormously from a deep cascade of short filters: on a USB capture, 1.370x
// for a single order-16 filter, 1.580x for 32/8, 1.808x for 16/8/4, and 1.889x
// for 8/8/4/2 -- which is also the cheapest of the deep configurations. A
// 12 kHz channel carrying a 2.65 kHz passband is about 4x oversampled, leaving
// structure at several scales for successive stages to remove. Wideband IQ
// gains nothing from the same treatment at twice the cost: against the 1.396x
// of one order-16 complex filter, 8/8/4/2 gave 1.391x and 16/8/4/2 gave 1.403x
// at more than double the CPU.
//
// # Why the packet declares its profile
//
// The header spends three bits it already had on the profile id, so
// self-description is free. A decoder reads the declaration and obeys it; it
// never inspects the sample rate or the channel count to guess a predictor.
//
// That keeps predictor choice a sender-side policy question. ProfileForChannels
// can be retuned -- for instance to give carrier-heavy bands a deeper cascade,
// worth 1.89x to 2.18x on a medium-wave capture -- without touching a single
// deployed decoder. Inferring the codec from the channel count would instead
// have frozen today's choice into every decoder permanently.
//
// # Why there are three profiles and not two
//
// Profiles 0 and 2 are the same complex filter. They differ only in that 2
// codes requantised samples and carries the shift in its body, and they are
// separate ids so that a decoder which does not implement the scale refuses the
// stream instead of playing it at a fraction of its amplitude. See scaled.go.
//
// Profile ids are consequently FIXED. A decoder that accepts this format is
// guaranteed to understand every profile a sender emits, so adding one is a
// format revision and never a silent change. That is why an unknown id is a
// hard error rather than a fallback to profile 0: decoding with the wrong
// predictor does not fail, it returns plausible-sounding noise.
type Profile struct {
	// ID is the value that travels on the wire.
	ID byte

	// Name is for logs and diagnostics. Nothing on the wire depends on it.
	Name string

	// Complex selects the filter form: true for interleaved I/Q, false for
	// mono. Dictated by the signal, not a tuning choice.
	Complex bool

	// Orders and Mus define the cascade, one entry per stage. Each stage
	// predicts the residual left by the stage before it. Mu is the sign-sign
	// step size in Q16 tap units: larger adapts faster but tracks more noisily.
	Orders []int
	Mus    []int64

	// Scaled reports that the body carries a leading shift byte and that the
	// decoded samples are scaled back up by that shift on the way out. It
	// changes nothing about the predictor: the filters run on the values on the
	// wire, on both sides, exactly as they do for an unscaled profile.
	Scaled bool
}

// Profile ids. These values are part of the wire format and are never
// reassigned.
const (
	// ProfileIQ is a single complex filter of order 16, for interleaved I/Q.
	ProfileIQ byte = 0

	// ProfileAudio is a four-stage real cascade, orders 8/8/4/2, for
	// demodulated mono audio.
	ProfileAudio byte = 1

	// ProfileIQScaled is ProfileIQ over requantised samples: the same complex
	// filter of order 16, with a power-of-two shift applied to the samples
	// before they reach the encoder's predictor and undone after they leave the
	// decoder's. The shift is the first byte of the body.
	//
	// This is the one profile whose output is not the samples that went in. It
	// exists because an IQ client that does not need the bottom bits should not
	// have to pay for them, and it is a separate profile id rather than a flag
	// precisely so that a decoder which never asked for it cannot be handed one
	// by accident: an unknown profile is a hard error everywhere, where an
	// unrecognised flag bit might be ignored. See SPEC.md section 6.6.
	ProfileIQScaled byte = 2
)

// MaxShift is the largest shift a scaled body may carry. Sixteen would leave
// nothing of an int16, and the depth selector in the reference sender never
// goes past 15, so anything above it is a corrupt or hostile packet rather than
// a stream a decoder should try to render.
const MaxShift = 15

// MaxStages bounds a cascade. Nothing in the format requires a bound; this is
// what the defined profiles need, and fixed-size storage in the C++ port
// depends on it.
const MaxStages = 4

var profiles = map[byte]Profile{
	ProfileIQ: {
		ID: ProfileIQ, Name: "iq-complex-o16", Complex: true,
		Orders: []int{16}, Mus: []int64{16},
	},
	ProfileAudio: {
		ID: ProfileAudio, Name: "audio-real-8/8/4/2", Complex: false,
		Orders: []int{8, 8, 4, 2}, Mus: []int64{16, 16, 32, 32},
	},
	ProfileIQScaled: {
		ID: ProfileIQScaled, Name: "iq-complex-o16-scaled", Complex: true,
		Orders: []int{16}, Mus: []int64{16}, Scaled: true,
	},
}

// LookupProfile returns the profile with the given id.
func LookupProfile(id byte) (Profile, error) {
	p, ok := profiles[id]
	if !ok {
		return Profile{}, fmt.Errorf("ubersdriq: unknown profile id %d", id)
	}
	return p, nil
}

// ProfileForChannels is the sender's default policy for which predictor to
// use. Nothing on the wire depends on it -- the packet declares the result --
// so a sender is free to replace it, including per band or per mode.
//
// It never returns ProfileIQScaled. Reduced depth is not something a channel
// count implies; it is something a client asks for, and a sender that has been
// asked calls StreamEncoder.EncodeScaled instead.
func ProfileForChannels(channels int) byte {
	if channels >= 2 {
		return ProfileIQ
	}
	return ProfileAudio
}
