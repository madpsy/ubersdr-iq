// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

// The conformance vectors.
//
// Each is a pair: a .pcm file of raw little-endian int16 samples and a .uiq
// file of the packets an encoder produced from them. Both directions are
// checked, and both matter for different reasons.
//
// Decoding is the correctness requirement: .uiq must reconstruct .pcm exactly.
// Any implementation that reads this format has to pass it.
//
// Encoding is the stricter one: .pcm must produce .uiq byte for byte. Nothing
// in the format requires that -- an encoder is free to choose a different k,
// take the escape earlier, or resynchronise more often, and its output would
// still decode correctly. But byte-identity is a far sharper test of a port
// than round-tripping, because it catches a predictor that is subtly wrong in
// a way that its own decoder reproduces and therefore hides. It is required of
// ports in this repository for that reason, not because the format demands it.
var vectors = []struct {
	name     string
	rate     int
	channels int
	frames   int // frames per packet, so int16 samples per packet is frames*channels
	what     string
}{
	{"audio-cw-12k", 12000, 1, 240, "a narrow CW tone in a mostly empty channel: the highest ratio the format reaches"},
	{"audio-usb-12k", 12000, 1, 240, "demodulated single-sideband, an ordinary voice-bandwidth channel"},
	{"iq-12k", 12000, 2, 240, "narrowband complex baseband"},
	{"iq-384k", 384000, 2, 360, "wideband complex baseband on a busy band: the throughput case"},
	{"iq-384k-mw", 384000, 2, 360, "medium wave, 60 dB between the carriers and the noise floor, which produces the longest unary runs in the set"},
	{"rice-63", 96000, 2, 240, "generated: a Rice codeword whose unary run empties a full 64-bit accumulator, which C and C++ get wrong and no captured signal here contains"},
}

func vectorPath(name, ext string) string {
	return filepath.Join("..", "..", "testdata", name+ext)
}

func readPCM(t *testing.T, path string) []int16 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

func readPackets(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sr := ubersdriq.NewStreamReader(bufio.NewReader(f))
	var out [][]byte
	for {
		pkt, err := sr.ReadPacket()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), pkt...))
	}
}

func TestVectorsDecodeExactly(t *testing.T) {
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			want := readPCM(t, vectorPath(v.name, ".pcm"))
			dec := ubersdriq.NewStreamDecoder()
			var got []int16
			var wire int
			for n, pkt := range readPackets(t, vectorPath(v.name, ".uiq")) {
				h, s, err := dec.Decode(pkt)
				if err != nil {
					t.Fatalf("packet %d: %v", n, err)
				}
				if h.SampleRate != v.rate || h.Channels != v.channels {
					t.Fatalf("packet %d: header says %d Hz / %d ch, vector is %d / %d",
						n, h.SampleRate, h.Channels, v.rate, v.channels)
				}
				got = append(got, s...)
				wire += len(pkt)
			}
			if len(got) != len(want) {
				t.Fatalf("decoded %d samples, vector has %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("sample %d: got %d, want %d -- not lossless", i, got[i], want[i])
				}
			}
			t.Logf("%d samples, %d -> %d bytes, %.3fx", len(want), len(want)*2, wire,
				float64(len(want)*2)/float64(wire))
		})
	}
}

func TestVectorsEncodeByteForByte(t *testing.T) {
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			samples := readPCM(t, vectorPath(v.name, ".pcm"))
			want := readPackets(t, vectorPath(v.name, ".uiq"))

			enc := ubersdriq.NewStreamEncoder()
			per := v.frames * v.channels
			// The same timestamp schedule the uiq tool uses, which is what the
			// committed vectors were produced with.
			const startNs = 1_700_000_000_000_000_000
			var n int
			for i := 0; i+per <= len(samples); i += per {
				ts := uint64(startNs) + uint64(i/v.channels)*1e9/uint64(v.rate)
				pkt, err := enc.Encode(samples[i:i+per], ts, v.rate, v.channels, -999, -999)
				if err != nil {
					t.Fatal(err)
				}
				if n >= len(want) {
					t.Fatalf("encoder produced more than the %d packets in the vector", len(want))
				}
				if !bytes.Equal(pkt, want[n]) {
					t.Fatalf("packet %d differs\n got %d bytes: %x\nwant %d bytes: %x",
						n, len(pkt), pkt[:min(32, len(pkt))], len(want[n]), want[n][:min(32, len(want[n]))])
				}
				n++
			}
			if n != len(want) {
				t.Fatalf("encoded %d packets, vector has %d", n, len(want))
			}
		})
	}
}

// The edge-case vector is generated rather than captured, so it is checked
// against the definition in vectors_test.go as well as decoded.
func TestEdgeCaseVector(t *testing.T) {
	wantPackets, wantSamples := edgeCaseStream(t)
	gotPackets := readPackets(t, vectorPath("edge-cases", ".uiq"))
	if len(gotPackets) != len(wantPackets) {
		t.Fatalf("committed vector has %d packets, the definition produces %d",
			len(gotPackets), len(wantPackets))
	}
	for i := range wantPackets {
		if !bytes.Equal(gotPackets[i], wantPackets[i]) {
			t.Fatalf("packet %d differs from the committed vector; regenerate with UIQ_WRITE_VECTORS=1", i)
		}
	}

	// And it must decode back to the samples that went in.
	dec := ubersdriq.NewStreamDecoder()
	var got []int16
	var silent, escape, resync, profiles int
	lastProfile := byte(0xff)
	for n, pkt := range gotPackets {
		if pkt[4]&ubersdriq.FlagMetadata != 0 {
			resync++
		}
		h, s, err := dec.Decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", n, err)
		}
		if h.Silent {
			silent++
		}
		if h.Escape {
			escape++
		}
		if h.Profile != lastProfile {
			profiles++
			lastProfile = h.Profile
		}
		got = append(got, s...)
	}
	if len(got) != len(wantSamples) {
		t.Fatalf("decoded %d samples, expected %d", len(got), len(wantSamples))
	}
	for i := range wantSamples {
		if got[i] != wantSamples[i] {
			t.Fatalf("sample %d: got %d, want %d", i, got[i], wantSamples[i])
		}
	}

	// The vector is only worth committing if it still contains every case it
	// claims to. This fails if a change to the generator quietly drops one.
	if silent == 0 || escape == 0 || resync < 2 || profiles < 2 {
		t.Errorf("edge-case vector no longer covers what it should: "+
			"%d silent, %d escape, %d resync, %d profile changes", silent, escape, resync, profiles)
	}
	t.Logf("%d packets: %d silent, %d escaped, %d resync points, %d profile changes",
		len(gotPackets), silent, escape, resync, profiles)
}

// The scaled-IQ vector, like the edge-case one, is generated rather than
// captured, so it is checked against its definition as well as decoded.
func TestScaledIQVector(t *testing.T) {
	wantPackets, wantSamples := scaledIQStream(t)
	gotPackets := readPackets(t, vectorPath("iq-scaled", ".uiq"))
	if len(gotPackets) != len(wantPackets) {
		t.Fatalf("committed vector has %d packets, the definition produces %d",
			len(gotPackets), len(wantPackets))
	}
	for i := range wantPackets {
		if !bytes.Equal(gotPackets[i], wantPackets[i]) {
			t.Fatalf("packet %d differs from the committed vector; regenerate with UIQ_WRITE_VECTORS=1", i)
		}
	}

	want := readPCM(t, vectorPath("iq-scaled", ".pcm"))
	dec := ubersdriq.NewStreamDecoder()
	// A second, independent header decoder, so the shift byte at the front of
	// each scaled body can be read without reaching inside the stream decoder.
	hdr := ubersdriq.NewHeaderDecoder()
	var got []int16
	var scaled, silent, escape, profileChanges int
	shifts := map[byte]int{}
	lastProfile := byte(0xff)
	for n, pkt := range gotPackets {
		hh, off, err := hdr.Decode(pkt)
		if err != nil {
			t.Fatalf("packet %d header: %v", n, err)
		}
		if hh.Profile == ubersdriq.ProfileIQScaled && !hh.Silent {
			shifts[pkt[off]]++
		}
		h, s, err := dec.Decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", n, err)
		}
		if h.Profile == ubersdriq.ProfileIQScaled {
			scaled++
		}
		if h.Silent {
			silent++
		}
		if h.Escape {
			escape++
		}
		if h.Profile != lastProfile {
			profileChanges++
			lastProfile = h.Profile
		}
		got = append(got, s...)
	}
	if len(got) != len(wantSamples) || len(got) != len(want) {
		t.Fatalf("decoded %d samples, definition has %d, .pcm has %d", len(got), len(wantSamples), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %d, want %d", i, got[i], want[i])
		}
	}

	// The vector is only worth committing if it still contains every case it
	// claims to: a shift of zero and several non-zero ones, a silent scaled
	// packet, an escaped scaled packet, and the crossings in and out of the
	// lossless profile.
	if _, ok := shifts[0]; !ok || len(shifts) < 4 ||
		scaled == 0 || silent == 0 || escape == 0 || profileChanges < 3 {
		t.Errorf("scaled-IQ vector no longer covers what it should: "+
			"%d scaled, %d silent, %d escape, %d profile changes, shifts %v",
			scaled, silent, escape, profileChanges, shifts)
	}
	t.Logf("%d packets: %d scaled, %d silent, %d escaped, %d profile changes, shifts %v",
		len(gotPackets), scaled, silent, escape, profileChanges, shifts)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
