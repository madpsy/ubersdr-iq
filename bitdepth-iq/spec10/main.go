// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins
//
// Command spec10 measures one stream, one arm, for SPEC.md §10.1 and §10.3.
//
// `uiq encode` already reports bytes, but three things it does not do are
// exactly what §10 now needs:
//
//   - Packetise the way the receiver actually does. A 384 kHz IQ block is 20 ms
//     of samples split into 21 packets of 360 frames and one of 120, not a
//     uniform 360; the header cost per sample differs between the two and the
//     Rice parameter is chosen per packet, so the pattern belongs in the
//     measurement rather than being rounded off.
//   - Drive the reduced-depth profile from a per-packet shift chosen outside,
//     which is where the margin policy lives (margin_rule.py). The shift is a
//     sender policy, not part of the format, so the format's own tools have no
//     business choosing it.
//   - Report the Rice parameter, which is §10.3's whole subject and is not
//     otherwise visible from outside the codec.
//
// The `-fixed` arm is §10.1's comparison figure: a fixed first-order predictor
// with the same Rice coder, in the same packets, behind the same header
// encoder. Only the predictor differs, which is the point of quoting it.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

// parsePattern turns "360x21,120" into the repeating run of frame counts one
// 20 ms receiver block is split into.
func parsePattern(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, reps := part, 1
		if i := strings.IndexByte(part, 'x'); i >= 0 {
			n = part[:i]
			r, err := strconv.Atoi(part[i+1:])
			if err != nil || r < 1 {
				return nil, fmt.Errorf("bad repeat in %q", part)
			}
			reps = r
		}
		v, err := strconv.Atoi(n)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("bad frame count in %q", part)
		}
		for i := 0; i < reps; i++ {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty pattern")
	}
	return out, nil
}

func readSamples(path string) ([]int16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("%s: %d bytes is not whole int16 samples", path, len(b))
	}
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s, nil
}

func writeSamples(path string, s []int16) error {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return os.WriteFile(path, b, 0o644)
}

// readCounts reads an explicit frame count per packet, one per line.
//
// A repeating pattern is the right thing when the packetisation is being
// reproduced from what the receiver was observed to do. When the capture IS the
// packets -- the length-prefixed version 3 captures in ka9q_ubersdr/testdata --
// the boundaries are already known exactly, and reproducing them beats
// inferring them.
func readCounts(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		v, err := strconv.Atoi(f)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("bad frame count %q", f)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no counts", path)
	}
	return out, nil
}

func readShifts(path string) ([]uint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []uint
	for _, f := range strings.Fields(string(b)) {
		v, err := strconv.Atoi(f)
		if err != nil || v < 0 || v > ubersdriq.MaxShift {
			return nil, fmt.Errorf("bad shift %q", f)
		}
		out = append(out, uint(v))
	}
	return out, nil
}

func zigzag(v int32) uint32 { return uint32(v<<1) ^ uint32(v>>31) }

// riceBytes is §6.3 as a length: the parameter byte plus the bitstream the
// codewords would occupy. Nothing here needs the bits themselves, and counting
// them is exact.
func riceBytes(res []int32) (int, int) {
	var sum uint64
	for _, r := range res {
		sum += uint64(zigzag(r))
	}
	mean := sum / uint64(len(res))
	k := 0
	if mean != 0 {
		k = 0
		for m := mean; m > 1; m >>= 1 {
			k++
		}
		if k > 30 {
			k = 30
		}
	}
	var bits uint64
	for _, r := range res {
		bits += uint64(zigzag(r)>>uint(k)) + 1 + uint64(k)
	}
	return 1 + int((bits+7)/8), k
}

type result struct {
	Arm          string         `json:"arm"`
	Rate         int            `json:"rate"`
	Channels     int            `json:"channels"`
	Packets      int            `json:"packets"`
	Samples      int            `json:"samples"`
	DurationS    float64        `json:"duration_s"`
	RawBytes     int            `json:"raw_bytes"`
	CodedBytes   int            `json:"coded_bytes"`
	HeaderBytes  int            `json:"header_bytes"`
	Escapes      int            `json:"escapes"`
	Silents      int            `json:"silents"`
	Resyncs      int            `json:"resyncs"`
	KMin         int            `json:"k_min"`
	KMax         int            `json:"k_max"`
	KHist        map[string]int `json:"k_hist"`
	RoundTrip    string         `json:"round_trip"`
	ShiftMin     int            `json:"shift_min"`
	ShiftMax     int            `json:"shift_max"`
	ShiftMean    float64        `json:"shift_mean"`
	FramesPerPkt map[string]int `json:"frames_per_packet"`
}

func main() {
	in := flag.String("in", "", "raw little-endian int16 input")
	rate := flag.Int("rate", 0, "sample rate in Hz")
	channels := flag.Int("channels", 1, "1 mono audio, 2 interleaved I/Q")
	pattern := flag.String("pattern", "240", "frames per packet, repeating: \"360x21,120\"")
	countPath := flag.String("counts", "", "explicit frame count per packet, one per line; overrides -pattern")
	shiftPath := flag.String("shifts", "", "one shift per packet; selects the scaled profile")
	fixed := flag.Bool("fixed", false, "fixed first-order predictor with the same Rice coder")
	outPCM := flag.String("out", "", "write the decoded samples here")
	outJSON := flag.String("json", "", "write the report here")
	startNs := flag.Uint64("start-ns", 1_700_000_000_000_000_000, "timestamp of the first sample")
	flag.Parse()
	if *in == "" || *rate <= 0 {
		flag.Usage()
		os.Exit(2)
	}

	pat, err := parsePattern(*pattern)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec10:", err)
		os.Exit(1)
	}
	// An explicit list is not a repeating pattern: it runs out, and the run
	// ends with it rather than wrapping round to the start again.
	var counts []int
	if *countPath != "" {
		if counts, err = readCounts(*countPath); err != nil {
			fmt.Fprintln(os.Stderr, "spec10:", err)
			os.Exit(1)
		}
	}
	samples, err := readSamples(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec10:", err)
		os.Exit(1)
	}
	var shifts []uint
	if *shiftPath != "" {
		if shifts, err = readShifts(*shiftPath); err != nil {
			fmt.Fprintln(os.Stderr, "spec10:", err)
			os.Exit(1)
		}
	}

	arm := "lossless"
	if *fixed {
		arm = "fixed-order1"
	} else if shifts != nil {
		arm = "scaled"
	}

	enc := ubersdriq.NewStreamEncoder()
	hdrEnc := ubersdriq.NewHeaderEncoder()
	dec := ubersdriq.NewStreamDecoder()
	hdrDec := ubersdriq.NewHeaderDecoder()

	r := result{
		Arm: arm, Rate: *rate, Channels: *channels,
		KMin: 1 << 30, KMax: -1,
		KHist: map[string]int{}, FramesPerPkt: map[string]int{},
	}
	var decoded []int16
	var expected []int16 // what the decoder must return
	var prevI, prevQ int64
	var shiftSum int
	r.ShiftMin, r.ShiftMax = 1<<30, -1

	pos, pi, frame := 0, 0, 0
	for {
		var n int
		if counts != nil {
			if pi >= len(counts) {
				break
			}
			n = counts[pi] * *channels
		} else {
			n = pat[pi%len(pat)] * *channels
		}
		pi++
		if pos+n > len(samples) {
			break
		}
		blk := samples[pos : pos+n]
		ts := *startNs + uint64(frame)*1e9/uint64(*rate)

		var pkt []byte
		switch {
		case *fixed:
			// Same header encoder, same packetisation, same Rice coder; the
			// only thing changed is the predictor.
			silent := true
			for _, v := range blk {
				if v != 0 {
					silent = false
					break
				}
			}
			res := make([]int32, len(blk))
			if *channels == 2 {
				for i := 0; i < len(blk); i += 2 {
					a, b := int64(blk[i]), int64(blk[i+1])
					res[i] = int32(a - prevI)
					res[i+1] = int32(b - prevQ)
					prevI, prevQ = a, b
				}
			} else {
				for i := range blk {
					a := int64(blk[i])
					res[i] = int32(a - prevI)
					prevI = a
				}
			}
			bodyLen, k := riceBytes(res)
			escape := bodyLen >= 2*len(blk)
			if escape {
				bodyLen = 2 * len(blk)
			}
			if silent {
				bodyLen = 0
			}
			h := ubersdriq.Header{
				TimestampNanos: ts, SampleRate: *rate, Channels: *channels,
				SampleCount: len(blk), BasebandPower: -999, Noise: -999,
				Profile: ubersdriq.ProfileForChannels(*channels),
				Escape:  escape, Silent: silent,
			}
			hdr := hdrEnc.Append(nil, h)
			r.HeaderBytes += len(hdr)
			r.CodedBytes += len(hdr) + bodyLen
			if !silent && !escape {
				r.KHist[strconv.Itoa(k)]++
				if k < r.KMin {
					r.KMin = k
				}
				if k > r.KMax {
					r.KMax = k
				}
			}
			if escape {
				r.Escapes++
			}
			if silent {
				r.Silents++
			}
		case shifts != nil:
			if r.Packets >= len(shifts) {
				fmt.Fprintf(os.Stderr, "spec10: only %d shifts for packet %d\n", len(shifts), r.Packets)
				os.Exit(1)
			}
			sh := shifts[r.Packets]
			shiftSum += int(sh)
			if int(sh) < r.ShiftMin {
				r.ShiftMin = int(sh)
			}
			if int(sh) > r.ShiftMax {
				r.ShiftMax = int(sh)
			}
			pkt, err = enc.EncodeScaled(blk, ts, *rate, *channels, -999, -999, sh)
			if err != nil {
				fmt.Fprintln(os.Stderr, "spec10:", err)
				os.Exit(1)
			}
			// What the decoder owes us: the quantised values, restored.
			q := append([]int16(nil), blk...)
			ubersdriq.Quantise(q, sh)
			ubersdriq.Restore(q, sh)
			expected = append(expected, q...)
		default:
			pkt, err = enc.Encode(blk, ts, *rate, *channels, -999, -999)
			if err != nil {
				fmt.Fprintln(os.Stderr, "spec10:", err)
				os.Exit(1)
			}
			expected = append(expected, blk...)
		}

		if pkt != nil {
			r.CodedBytes += len(pkt)
			h, hlen, err := hdrDec.Decode(pkt)
			if err != nil {
				fmt.Fprintln(os.Stderr, "spec10: header:", err)
				os.Exit(1)
			}
			r.HeaderBytes += hlen
			if pkt[4]&ubersdriq.FlagMetadata != 0 {
				r.Resyncs++
			}
			if h.Silent {
				r.Silents++
			} else if h.Escape {
				r.Escapes++
			} else {
				kOff := hlen
				if h.Profile == ubersdriq.ProfileIQScaled {
					kOff++
				}
				k := int(pkt[kOff])
				r.KHist[strconv.Itoa(k)]++
				if k < r.KMin {
					r.KMin = k
				}
				if k > r.KMax {
					r.KMax = k
				}
			}
			_, got, err := dec.Decode(pkt)
			if err != nil {
				fmt.Fprintln(os.Stderr, "spec10: decode:", err)
				os.Exit(1)
			}
			decoded = append(decoded, got...)
		}

		r.Packets++
		r.RawBytes += 2 * len(blk)
		r.Samples += len(blk)
		r.FramesPerPkt[strconv.Itoa(len(blk) / *channels)]++
		pos += n
		frame += len(blk) / *channels
	}

	r.DurationS = float64(r.Samples) / float64(*channels) / float64(*rate)
	if r.KMax < 0 {
		r.KMin, r.KMax = -1, -1
	}
	if r.ShiftMax < 0 {
		r.ShiftMin, r.ShiftMax = 0, 0
	} else {
		r.ShiftMean = float64(shiftSum) / float64(r.Packets)
	}

	r.RoundTrip = "not applicable"
	if !*fixed {
		r.RoundTrip = "exact"
		if len(decoded) != len(expected) {
			r.RoundTrip = fmt.Sprintf("LENGTH MISMATCH %d != %d", len(decoded), len(expected))
		} else {
			for i := range decoded {
				if decoded[i] != expected[i] {
					r.RoundTrip = fmt.Sprintf("MISMATCH at sample %d", i)
					break
				}
			}
		}
		if r.RoundTrip != "exact" {
			fmt.Fprintln(os.Stderr, "spec10: ROUND TRIP FAILED:", r.RoundTrip)
			os.Exit(1)
		}
		if *outPCM != "" {
			if err := writeSamples(*outPCM, decoded); err != nil {
				fmt.Fprintln(os.Stderr, "spec10:", err)
				os.Exit(1)
			}
		}
	}

	b, _ := json.MarshalIndent(r, "", " ")
	if *outJSON != "" {
		if err := os.WriteFile(*outJSON, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "spec10:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("%-12s %7d pkts %11d -> %10d B  %.3fx  k %d-%d  esc %d  sil %d  hdr %.2f B/pkt  %s\n",
		arm, r.Packets, r.RawBytes, r.CodedBytes,
		float64(r.RawBytes)/float64(r.CodedBytes), r.KMin, r.KMax,
		r.Escapes, r.Silents, float64(r.HeaderBytes)/float64(r.Packets), r.RoundTrip)
}
