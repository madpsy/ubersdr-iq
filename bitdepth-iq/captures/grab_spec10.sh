#!/bin/bash
# Capture the corpus SPEC.md §10.1 is measured on. Sequential on purpose, the
# same reason grab.sh is: one session at a time so a wideband stream never
# shares the link with another, and so the receiver -- a shared production
# service -- carries one of our sessions at a time and no more.
#
# The demodulated rows come off the audio socket (grab_audio.py) because
# iq-recorder only speaks the IQ modes. The IQ 384 kHz rows for a quiet and a
# busy band are NOT here: captures/20m-384 and captures/quiet-6m from
# 2026-09-03 already are those, they are raw receiver output, and nothing since
# has changed what the receiver sends.
#
# Frequencies are not fixtures. Each was chosen from the receiver's own live
# readings minutes before the run -- CW from /api/cwskimmer/spots, voice from
# /api/noisefloor/voice-activity/all, the medium wave and CB carriers from
# /api/noisefloor/fft/wideband -- and every capture is verified afterwards to
# contain what it was aimed at. Re-running this blind, months later, measures
# whatever happens to be there instead.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$HERE/audio
HOST=${UBERSDR_HOST:?set UBERSDR_HOST}
mkdir -p "$OUT"

aud() {  # name freq mode duration [extra...]
  echo "=== $1  $2 Hz  $3  $4 s  (start $(date -u +%H:%M:%S)Z)"
  python3 "$HERE/grab_audio.py" -host $HOST -freq "$2" -mode "$3" \
      -duration "$4" -out "$OUT/$1" "${@:5}"
}

# --- demodulated audio, 12 kHz -------------------------------------------
aud cw       14033000 cwu 30      # keyed carrier, ±200 Hz CW filter
aud ssbvoice  7152000 lsb 30      # 40 m phone
aud ssbdata  14074000 usb 60      # FT8; 60 s so whole 15 s periods survive
aud squelch  14300000 usb 30 -min-snr 50   # gate held shut: digital silence

# --- demodulated audio, 24 kHz -------------------------------------------
aud am         810000 am  30      # medium wave broadcast
aud nfm      27781000 nfm 30      # 27 MHz FM

# --- IQ ------------------------------------------------------------------
aud iq12     14175000 iq    30    # 12 kHz IQ
aud iq384mw    909000 iq384 30    # 384 kHz IQ over the medium wave band
echo "=== done $(date -u +%H:%M:%S)Z"

# --- the IQ rate ladder ---------------------------------------------------
# All five IQ modes the server offers, at one frequency, back to back, so a
# reader comparing rates is comparing rates and not bands. 14.175 MHz because
# the 192 and 384 kHz captures already on disk are centred there, and because
# it is the receiver's own default: 20 m phone, occupied at every hour this
# receiver is up.
#
# `iq` is a mode in its own right, not a rate of the others: 12 kHz from
# presets.conf, requestable by any client where iq48..iq384 need a bypass, and
# the only one of the five that takes filter edges. ±6 kHz is what the
# reference client sends for it; the wide modes keep their preset bandwidth and
# ignore the parameter, so it is passed to `iq` alone.
aud lad-iq    14175000 iq    30 -bwlow -6000 -bwhigh 6000
aud lad-iq48  14175000 iq48  30
aud lad-iq96  14175000 iq96  30
aud lad-iq192 14175000 iq192 30
aud lad-iq384 14175000 iq384 30
