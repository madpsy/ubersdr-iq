// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// Per-packet cost benchmark for SPEC.md section 10.2, the Go arm.
//
// Deliberately the same shape as bench-cost/bench.cpp: one codec instance per
// repeat so the predictor adaptation carries packet to packet, two warmup
// repeats discarded, median over the rest.
//
//	go run . FILE.pcm RATE CHANNELS FRAMES_PER_PACKET SHIFT PACKETS REPEATS
//
// SHIFT < 0 selects the lossless profile; SHIFT >= 0 selects profile 2.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"time"

	uiq "github.com/madpsy/ubersdr-iq/go/ubersdriq"
)

func median(v []float64) (med, lo, hi float64) {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		med = s[n/2]
	} else {
		med = 0.5 * (s[n/2-1] + s[n/2])
	}
	return med, s[0], s[n-1]
}

func main() {
	if len(os.Args) < 8 {
		fmt.Fprintf(os.Stderr, "usage: %s FILE.pcm RATE CHANNELS FRAMES SHIFT PACKETS REPEATS\n", os.Args[0])
		os.Exit(2)
	}
	path := os.Args[1]
	atoi := func(i int) int { n, _ := strconv.Atoi(os.Args[i]); return n }
	rate, channels, frames := atoi(2), atoi(3), atoi(4)
	shift, packets, repeats := atoi(5), atoi(6), atoi(7)

	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	src := make([]int16, len(raw)/2)
	for i := range src {
		src[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	raw = nil

	per := frames * channels
	need := per * packets
	if len(src) < per {
		fmt.Fprintln(os.Stderr, "file shorter than one packet")
		os.Exit(1)
	}
	tiles := 1
	var samples []int16
	if len(src) >= need {
		samples = src[:need]
	} else {
		whole := (len(src) / per) * per
		if whole == 0 {
			whole = per
		}
		samples = make([]int16, 0, need)
		for len(samples) < need {
			take := whole
			if r := need - len(samples); r < take {
				take = r
			}
			samples = append(samples, src[:take]...)
		}
		tiles = (need + whole - 1) / whole
	}

	scaled := shift >= 0

	// The GC is part of what a Go deployment pays, so it is left on; it is
	// only stopped from running between the encode and decode phases at a
	// point that would land inside a timed region by chance.
	debug.SetGCPercent(100)

	stream := make([][]byte, 0, packets)
	encUs := make([]float64, 0, repeats)
	for r := 0; r < repeats+2; r++ {
		enc := uiq.NewStreamEncoder()
		keep := r == 0
		runtime.GC()
		t0 := time.Now()
		for p := 0; p < packets; p++ {
			ts := uint64(1700000000000000000) + uint64(p)*uint64(frames)*1e9/uint64(rate)
			s := samples[p*per : (p+1)*per]
			var pkt []byte
			var err error
			if scaled {
				pkt, err = enc.EncodeScaled(s, ts, rate, channels, -999, -999, uint(shift))
			} else {
				pkt, err = enc.Encode(s, ts, rate, channels, -999, -999)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "encode:", err)
				os.Exit(1)
			}
			if keep {
				stream = append(stream, append([]byte(nil), pkt...))
			}
		}
		d := time.Since(t0)
		if r >= 2 {
			encUs = append(encUs, float64(d.Nanoseconds())/1e3/float64(packets))
		}
	}

	wire := 0
	for _, p := range stream {
		wire += len(p)
	}

	decUs := make([]float64, 0, repeats)
	for r := 0; r < repeats+2; r++ {
		dec := uiq.NewStreamDecoder()
		runtime.GC()
		t0 := time.Now()
		for p := 0; p < packets; p++ {
			if _, _, err := dec.Decode(stream[p]); err != nil {
				fmt.Fprintf(os.Stderr, "decode packet %d: %v\n", p, err)
				os.Exit(1)
			}
		}
		d := time.Since(t0)
		if r >= 2 {
			decUs = append(decUs, float64(d.Nanoseconds())/1e3/float64(packets))
		}
	}

	em, elo, ehi := median(encUs)
	dm, dlo, dhi := median(decUs)
	pps := float64(rate) / float64(frames)
	bits := 8.0 * float64(wire) / float64(packets*per)

	fmt.Printf("{\"impl\":\"go\",\"file\":%q,\"rate\":%d,\"channels\":%d,\"frames\":%d,"+
		"\"shift\":%d,\"packets\":%d,\"repeats\":%d,\"tiles\":%d,\"pps\":%.2f,"+
		"\"bits_per_sample\":%.3f,"+
		"\"enc_us\":%.3f,\"enc_us_lo\":%.3f,\"enc_us_hi\":%.3f,"+
		"\"dec_us\":%.3f,\"dec_us_lo\":%.3f,\"dec_us_hi\":%.3f,"+
		"\"enc_core_pct\":%.4f,\"dec_core_pct\":%.4f,"+
		"\"enc_msps\":%.3f,\"dec_msps\":%.3f}\n",
		path, rate, channels, frames, shift, packets, repeats, tiles, pps, bits,
		em, elo, ehi, dm, dlo, dhi,
		em*pps/1e4, dm*pps/1e4, float64(per)/em, float64(per)/dm)
}
