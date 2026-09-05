// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

package ubersdriq

import (
	"encoding/binary"
	"fmt"
	"io"
)

// A stream file.
//
// The packet format says nothing about where one packet ends and the next
// begins, because on the transport it comes from -- a WebSocket -- the frame
// boundary already does. Storing packets in a file needs that boundary back,
// so this defines the minimum that supplies it:
//
//	[length u32 little-endian][packet bytes]  repeated to end of file
//
// There is no file header and no index. The convention is the extension .uiq.
//
// This container is a convenience, not part of the format: an implementation
// that carries packets some other way is still speaking UberSDR-IQ. It is
// defined here so that conformance vectors, the reference tools and the
// examples all agree on one thing.
//
// A file is read from its first byte. There is no seeking to an arbitrary
// packet: predictor state is never retransmitted, so a decoder that starts
// part-way through a stream produces wrong samples indefinitely. A container
// that needs random access has to cut the input into independent streams and
// index them, accepting one cold start per entry point.

// StreamWriter writes length-prefixed packets.
type StreamWriter struct {
	w   io.Writer
	buf [4]byte
}

// NewStreamWriter wraps w.
func NewStreamWriter(w io.Writer) *StreamWriter { return &StreamWriter{w: w} }

// WritePacket writes one packet with its length prefix.
func (sw *StreamWriter) WritePacket(pkt []byte) error {
	if len(pkt) == 0 {
		return fmt.Errorf("ubersdriq: refusing to write an empty packet")
	}
	binary.LittleEndian.PutUint32(sw.buf[:], uint32(len(pkt)))
	if _, err := sw.w.Write(sw.buf[:]); err != nil {
		return err
	}
	_, err := sw.w.Write(pkt)
	return err
}

// StreamReader reads length-prefixed packets.
type StreamReader struct {
	r   io.Reader
	buf []byte
}

// NewStreamReader wraps r.
func NewStreamReader(r io.Reader) *StreamReader { return &StreamReader{r: r} }

// ReadPacket returns the next packet, or io.EOF at the end of the file. The
// returned slice is reused by the next call.
func (sr *StreamReader) ReadPacket() ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(sr.r, l[:]); err != nil {
		// io.EOF here is a clean end of file; anything else is a truncated
		// prefix, which io.ReadFull already reports as ErrUnexpectedEOF.
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(l[:]))
	if n <= 0 || n > 1<<26 {
		return nil, fmt.Errorf("ubersdriq: implausible packet length %d", n)
	}
	if cap(sr.buf) < n {
		sr.buf = make([]byte, n)
	}
	sr.buf = sr.buf[:n]
	if _, err := io.ReadFull(sr.r, sr.buf); err != nil {
		return nil, fmt.Errorf("ubersdriq: truncated packet: %w", err)
	}
	return sr.buf, nil
}
