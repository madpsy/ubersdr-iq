// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// Command uiq encodes, decodes and reports on UberSDR-IQ streams.
//
// Sample files are raw little-endian int16, interleaved I/Q for two channels
// -- the same layout as the body of a WAV file, so `sox`, `ffmpeg` or a WAV
// header can be put in front of one without touching the samples. Stream files
// are the length-prefixed container described in container.go.
//
//	uiq encode -rate 12000 -channels 1 -frames 240 in.pcm out.uiq
//	uiq decode out.uiq back.pcm
//	uiq stat out.uiq
//	uiq bench in.pcm -rate 384000 -channels 2 -frames 360
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "encode":
		err = encode(os.Args[2:])
	case "decode":
		err = decode(os.Args[2:])
	case "stat":
		err = stat(os.Args[2:])
	case "bench":
		err = bench(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "uiq:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  uiq encode -rate HZ [-channels N] [-frames N] [-start-ns N] in.pcm out.uiq
  uiq decode in.uiq out.pcm
  uiq stat in.uiq
  uiq bench -rate HZ [-channels N] [-frames N] in.pcm

Sample files are raw little-endian int16, interleaved for two channels.`)
	os.Exit(2)
}

// readSamples reads a raw little-endian int16 file.
func readSamples(path string) ([]int16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("%s: %d bytes is not a whole number of int16 samples", path, len(b))
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

type encodeFlags struct {
	rate     int
	channels int
	frames   int
	startNs  uint64
}

func (f *encodeFlags) bind(fs *flag.FlagSet) {
	fs.IntVar(&f.rate, "rate", 0, "sample rate in Hz (required)")
	fs.IntVar(&f.channels, "channels", 1, "1 for mono audio, 2 for interleaved I/Q")
	fs.IntVar(&f.frames, "frames", 240, "frames per packet")
	// A fixed default so that encoding the same input twice produces
	// byte-identical output, which is what makes conformance vectors possible.
	fs.Uint64Var(&f.startNs, "start-ns", 1_700_000_000_000_000_000, "timestamp of the first sample, nanoseconds")
}

func encode(args []string) error {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	var f encodeFlags
	f.bind(fs)
	fs.Parse(args)
	if f.rate <= 0 || fs.NArg() != 2 {
		usage()
	}
	samples, err := readSamples(fs.Arg(0))
	if err != nil {
		return err
	}
	out, err := os.Create(fs.Arg(1))
	if err != nil {
		return err
	}
	defer out.Close()
	bw := bufio.NewWriter(out)
	sw := ubersdriq.NewStreamWriter(bw)

	enc := ubersdriq.NewStreamEncoder()
	per := f.frames * f.channels
	if per <= 0 {
		return errors.New("frames and channels must be positive")
	}
	var packets, raw, coded int
	for i := 0; i+per <= len(samples); i += per {
		// The timestamp advances by exactly one packet of frames, which is
		// what a real receiver delivering at a steady rate produces.
		ts := f.startNs + uint64(i/f.channels)*1e9/uint64(f.rate)
		pkt, err := enc.Encode(samples[i:i+per], ts, f.rate, f.channels, -999, -999)
		if err != nil {
			return err
		}
		if err := sw.WritePacket(pkt); err != nil {
			return err
		}
		packets++
		raw += per * 2
		coded += len(pkt)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	fmt.Printf("%d packets, %d -> %d bytes, %.3fx\n", packets, raw, coded, float64(raw)/float64(coded))
	return nil
}

func decode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 2 {
		usage()
	}
	in, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer in.Close()
	sr := ubersdriq.NewStreamReader(bufio.NewReader(in))
	dec := ubersdriq.NewStreamDecoder()

	var all []int16
	for n := 0; ; n++ {
		pkt, err := sr.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		_, samples, err := dec.Decode(pkt)
		if err != nil {
			return fmt.Errorf("packet %d: %w", n, err)
		}
		all = append(all, samples...)
	}
	fmt.Printf("%d samples\n", len(all))
	return writeSamples(fs.Arg(1), all)
}

func stat(args []string) error {
	fs := flag.NewFlagSet("stat", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	in, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer in.Close()
	sr := ubersdriq.NewStreamReader(bufio.NewReader(in))
	dec := ubersdriq.NewStreamDecoder()

	var packets, raw, wire, header int
	var resyncs, escapes, silents int
	var first, last ubersdriq.Header
	for n := 0; ; n++ {
		pkt, err := sr.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		// The metadata flag is the resynchronisation marker; read it before
		// decoding, which is the only way to count them from outside.
		if pkt[4]&ubersdriq.FlagMetadata != 0 {
			resyncs++
		}
		h, samples, err := dec.Decode(pkt)
		if err != nil {
			return fmt.Errorf("packet %d: %w", n, err)
		}
		if n == 0 {
			first = h
		}
		last = h
		if h.Escape {
			escapes++
		}
		if h.Silent {
			silents++
		}
		packets++
		raw += len(samples) * 2
		wire += len(pkt)
		// Body length is unknown from outside, but the header length is
		// whatever the packet has beyond the body, and for a silent packet the
		// whole packet is header.
		if h.Silent {
			header += len(pkt)
		}
	}
	if packets == 0 {
		return errors.New("no packets")
	}
	dur := float64(last.TimestampNanos-first.TimestampNanos) / 1e9
	fmt.Printf("packets     %d\n", packets)
	fmt.Printf("rate        %d Hz, %d channel(s)\n", last.SampleRate, last.Channels)
	fmt.Printf("profile     %d\n", last.Profile)
	fmt.Printf("samples     %d (%.3f s of signal)\n", raw/2, dur)
	fmt.Printf("raw         %d bytes\n", raw)
	fmt.Printf("stream      %d bytes\n", wire)
	fmt.Printf("ratio       %.3fx (%.1f%% saved)\n", float64(raw)/float64(wire), 100*(1-float64(wire)/float64(raw)))
	fmt.Printf("resyncs     %d\n", resyncs)
	fmt.Printf("escapes     %d\n", escapes)
	fmt.Printf("silent      %d\n", silents)
	if dur > 0 {
		fmt.Printf("bitrate     %.1f kB/s raw, %.1f kB/s coded\n",
			float64(raw)/dur/1000, float64(wire)/dur/1000)
	}
	return nil
}

// bench reports encode and decode cost. The interesting figure is not
// bytes per second but the fraction of a core one stream costs, since a
// receiver serves many at once.
func bench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	var f encodeFlags
	f.bind(fs)
	reps := fs.Int("reps", 5, "passes over the input")
	fs.Parse(args)
	if f.rate <= 0 || fs.NArg() != 1 {
		usage()
	}
	samples, err := readSamples(fs.Arg(0))
	if err != nil {
		return err
	}
	per := f.frames * f.channels
	var packets [][]int16
	for i := 0; i+per <= len(samples); i += per {
		packets = append(packets, samples[i:i+per])
	}
	if len(packets) == 0 {
		return errors.New("input is shorter than one packet")
	}

	t0 := time.Now()
	var coded [][]byte
	for r := 0; r < *reps; r++ {
		enc := ubersdriq.NewStreamEncoder()
		coded = coded[:0]
		for i, p := range packets {
			ts := f.startNs + uint64(i*f.frames)*1e9/uint64(f.rate)
			pkt, err := enc.Encode(p, ts, f.rate, f.channels, -999, -999)
			if err != nil {
				return err
			}
			coded = append(coded, append([]byte(nil), pkt...))
		}
	}
	encPer := float64(time.Since(t0).Microseconds()) / float64(*reps*len(packets))

	t1 := time.Now()
	for r := 0; r < *reps; r++ {
		dec := ubersdriq.NewStreamDecoder()
		for _, pkt := range coded {
			if _, _, err := dec.Decode(pkt); err != nil {
				return err
			}
		}
	}
	decPer := float64(time.Since(t1).Microseconds()) / float64(*reps*len(packets))

	pps := float64(f.rate) / float64(f.frames)
	fmt.Printf("%d packets of %d frames at %d Hz (%.1f packets/s)\n", len(packets), f.frames, f.rate, pps)
	fmt.Printf("encode  %6.1f us/packet  %5.2f%% of a core  %6.2f Msample/s\n",
		encPer, encPer*pps/1e4, float64(per)/encPer)
	fmt.Printf("decode  %6.1f us/packet  %5.2f%% of a core  %6.2f Msample/s\n",
		decPer, decPer*pps/1e4, float64(per)/decPer)
	return nil
}
