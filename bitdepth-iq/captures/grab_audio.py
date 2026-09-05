#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Capture a demodulated-audio (or narrowband IQ) stream from a live receiver.

The IQ half of the §10.1 corpus comes from `iq-recorder`, which only speaks the
receiver's IQ modes. The demodulated rows need the same thing for `usb`, `cwu`,
`am`, `nfm` and a gated (squelched) stream, so this asks for the same socket the
browser does and writes what came off it.

Deliberately asks for protocol version 3, the zstd wrapper, rather than version
4. Version 4 *is* the codec under measurement, and taking the corpus through it
would make the measurement circular; version 3 carries the receiver's int16
samples with nothing done to them but a general-purpose compressor over the top,
which unzips back to exactly what radiod produced.

Two things are recorded beside the samples, because §10.1 needs both:

- the per-message frame count, which is the packetisation the server actually
  uses at that rate -- the thing the encoder must be handed to make a
  whole-packet figure mean anything;
- the zstd byte count, which is §10.1's own comparison figure for this stream,
  measured on the wire rather than reconstructed afterwards.

    grab_audio.py -host H -freq HZ -mode usb -duration 30 -out DIR/NAME
"""

import os
import argparse
import json
import struct
import sys
import time
import urllib.request
import uuid

import numpy as np
import zstandard
import websocket

UA = "UberSDR-IQ spec10 bench"

FULL_MAGIC = 0x5043      # "PC", full header
MIN_MAGIC = 0x504D       # "PM", minimal header
FULL_V1, FULL_V23, MINIMAL = 29, 37, 13

# The web UI's per-mode filter defaults (static/local-bookmarks-ui.js), so a row
# in the table is the stream a browser would have been served.
BW_DEFAULTS = {
    "usb": (50, 2700), "lsb": (-2700, -50),
    "cwu": (-200, 200), "cwl": (-200, 200),
    "am": (-5000, 5000), "sam": (-5000, 5000),
    "fm": (-8000, 8000), "nfm": (-5000, 5000),
}


def parse_packet(buf):
    """One decompressed message to (samples, meta). Samples are int16 host order."""
    magic = struct.unpack_from("<H", buf, 0)[0]
    if magic == MIN_MAGIC:
        return np.frombuffer(buf[MINIMAL:], dtype=">i2"), {}
    if magic != FULL_MAGIC:
        raise ValueError(f"bad magic 0x{magic:04x}")
    version = buf[2]
    hdr = FULL_V1 if version == 1 else FULL_V23
    rtp, wall_ms = struct.unpack_from("<QQ", buf, 4)
    rate, ch = struct.unpack_from("<IB", buf, 20)
    meta = {"version": version, "rate": rate, "channels": ch,
            "rtp": rtp, "wall_ms": wall_ms}
    if version >= 2:
        bb, nz = struct.unpack_from("<ff", buf, 25)
        meta["baseband_dbfs"], meta["noise_dbfs"] = bb, nz
    return np.frombuffer(buf[hdr:], dtype=">i2"), meta


def register(host, port, session_id):
    """POST /connection, which is what binds the session id to this User-Agent.

    The audio socket refuses a session id it has not seen come through here, so
    this is not optional politeness -- it is the handshake.
    """
    req = urllib.request.Request(
        f"http://{host}:{port}/connection",
        data=json.dumps({"user_session_id": session_id, "password": ""}).encode(),
        headers={"Content-Type": "application/json", "User-Agent": UA},
        method="POST")
    with urllib.request.urlopen(req, timeout=10) as r:
        info = json.load(r)
    if not info.get("allowed"):
        raise SystemExit(f"connection refused: {info.get('reason')}")
    return info


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-host", default=os.environ.get("UBERSDR_HOST", "ubersdr.local"))
    ap.add_argument("-port", type=int, default=8080)
    ap.add_argument("-freq", type=int, required=True)
    ap.add_argument("-mode", required=True)
    ap.add_argument("-duration", type=float, default=30.0)
    ap.add_argument("-out", required=True, help="output prefix, .pcm/.json appended")
    ap.add_argument("-min-snr", type=float, default=None,
                    help="audio gate threshold in dB; a value nothing reaches "
                         "holds the gate shut, which is the squelched row")
    ap.add_argument("-bwlow", type=int, default=None)
    ap.add_argument("-bwhigh", type=int, default=None)
    a = ap.parse_args()

    lo, hi = BW_DEFAULTS.get(a.mode, (None, None))
    if a.bwlow is not None:
        lo = a.bwlow
    if a.bwhigh is not None:
        hi = a.bwhigh

    sid = str(uuid.uuid4())
    conn = register(a.host, a.port, sid)

    q = [f"frequency={a.freq}", f"mode={a.mode}", "format=pcm-zstd", "version=3",
         f"user_session_id={sid}"]
    if lo is not None:
        q += [f"bandwidthLow={lo}", f"bandwidthHigh={hi}"]
    if a.min_snr is not None:
        q.append(f"min_snr={a.min_snr}")
    url = f"ws://{a.host}:{a.port}/ws?" + "&".join(q)

    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    print(f"    {url}")
    ws = websocket.create_connection(url, timeout=20, header=[f"User-Agent: {UA}"])
    dctx = zstandard.ZstdDecompressor()

    chunks, counts, wire, zbytes, meta = [], {}, 0, 0, {}
    quality = []
    first_wall = None
    t_end = time.time() + a.duration
    try:
        while time.time() < t_end:
            ws.settimeout(max(1.0, t_end - time.time()) + 5)
            m = ws.recv()
            if isinstance(m, str):
                msg = json.loads(m)
                if msg.get("type") == "error":
                    raise SystemExit(f"server error: {msg.get('error')}")
                continue                      # other JSON control message
            zbytes += len(m)
            raw = dctx.decompress(m, max_output_size=1 << 22)
            wire += len(raw)
            s, md = parse_packet(raw)
            if md:
                meta.update({k: v for k, v in md.items()
                             if k in ("version", "rate", "channels")})
                if first_wall is None:
                    first_wall = md["wall_ms"]
                if "baseband_dbfs" in md:
                    quality.append((md["baseband_dbfs"], md["noise_dbfs"]))
            chunks.append(s.astype("<i2"))
            counts[len(s)] = counts.get(len(s), 0) + 1
    finally:
        ws.close()

    x = np.concatenate(chunks) if chunks else np.zeros(0, dtype="<i2")
    x.tofile(a.out + ".pcm")
    ch = meta.get("channels", 1)
    frames = {str(n // ch): c for n, c in sorted(counts.items())}
    info = {
        "host": a.host, "frequency": a.freq, "mode": a.mode,
        "bypassed": conn.get("bypassed"),
        "allowed_iq_modes": conn.get("allowed_iq_modes"),
        "bandwidth_low": lo, "bandwidth_high": hi, "min_snr": a.min_snr,
        "started_utc": started,
        "first_packet_wall_ms": first_wall,
        "first_packet_utc": (time.strftime("%Y-%m-%dT%H:%M:%S",
                                          time.gmtime(first_wall / 1000.0))
                             + f".{first_wall % 1000:03d}Z") if first_wall else None,
        "finished_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "requested_duration_s": a.duration,
        "sample_rate": meta.get("rate"), "channels": ch,
        "protocol_version": meta.get("version"),
        "messages": len(chunks), "samples": int(len(x)),
        "duration_s": (len(x) / ch / meta["rate"]) if meta.get("rate") else None,
        "frames_per_packet": frames,
        "header_plus_pcm_bytes": wire,     # what the codec's raw+header compares to
        "zstd_wire_bytes": zbytes,         # §10.1's zstd comparison, on the wire
        "peak": int(np.max(np.abs(x.astype(np.int32)))) if len(x) else 0,
        "rms": float(np.sqrt(np.mean(x.astype(np.float64) ** 2))) if len(x) else 0.0,
        "nonzero_fraction": float(np.count_nonzero(x) / len(x)) if len(x) else 0.0,
        "baseband_dbfs_mean": float(np.mean([p for p, _ in quality])) if quality else None,
        "noise_dbfs_mean": float(np.mean([n for _, n in quality])) if quality else None,
    }
    with open(a.out + ".json", "w") as f:
        json.dump(info, f, indent=1)
    print(f"    {info['messages']} msgs, {info['samples']} samples, "
          f"{info['duration_s']:.2f} s at {info['sample_rate']} Hz x{ch}, "
          f"frames/packet {frames}, peak {info['peak']}, rms {info['rms']:.1f}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
