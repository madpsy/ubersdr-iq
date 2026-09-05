#!/usr/bin/env python3
"""Capture UberSDR-IQ (pcmv4) packets from a live UberSDR server to a .uiq file."""
import json, os, sys, time, uuid, struct, urllib.request
import websocket

HOST = os.environ.get("UBERSDR_HOST", "localhost")
PORT = int(os.environ.get("UBERSDR_PORT", "8080"))
MODE = sys.argv[1] if len(sys.argv) > 1 else "iq48"
FREQ = int(sys.argv[2]) if len(sys.argv) > 2 else 14080000
SECS = float(sys.argv[3]) if len(sys.argv) > 3 else 30.0
OUT  = sys.argv[4] if len(sys.argv) > 4 else "capture.uiq"

sid = str(uuid.uuid4())

# POST /connection (permission check), as the SDR++ ubersdr_source does
try:
    req = urllib.request.Request(
        f"http://{HOST}:{PORT}/connection",
        data=json.dumps({"user_session_id": sid}).encode(),
        headers={"Content-Type": "application/json", "User-Agent": "ubersdr-iq-bench/1.0"},
        method="POST")
    with urllib.request.urlopen(req, timeout=8) as r:
        print("/connection ->", r.status, r.read()[:300].decode(errors="replace"), file=sys.stderr)
except Exception as e:
    print("/connection failed (proceeding):", e, file=sys.stderr)

path = f"/ws?mode={MODE}&frequency={FREQ}&format=pcm-zstd&version=4&user_session_id={sid}"
url = f"ws://{HOST}:{PORT}{path}"
print("connecting:", url, file=sys.stderr)

ws = websocket.create_connection(url, timeout=15,
                                 header=["User-Agent: ubersdr-iq-bench/1.0"],
                                 origin=f"http://{HOST}:{PORT}")
ws.settimeout(15)

nbin = ntext = 0
t0 = None
deadline = None
with open(OUT, "wb") as f:
    while True:
        try:
            op, data = ws.recv_data(control_frame=False)
        except websocket.WebSocketTimeoutException:
            print("timeout waiting for data", file=sys.stderr); break
        except Exception as e:
            print("recv error:", e, file=sys.stderr); break
        if op == websocket.ABNF.OPCODE_TEXT:
            ntext += 1
            if ntext <= 6:
                print("TEXT:", data.decode(errors="replace")[:400], file=sys.stderr)
            continue
        if op != websocket.ABNF.OPCODE_BINARY:
            continue
        if t0 is None:
            t0 = time.time(); deadline = t0 + SECS
            print(f"first binary frame: {len(data)} bytes, magic={data[:4].hex()}", file=sys.stderr)
        nbin += 1
        f.write(struct.pack("<I", len(data)))
        f.write(data)
        if time.time() >= deadline:
            break
try:
    ws.close()
except Exception:
    pass
el = (time.time() - t0) if t0 else 0
print(f"captured {nbin} binary frames in {el:.2f}s -> {OUT}", file=sys.stderr)
