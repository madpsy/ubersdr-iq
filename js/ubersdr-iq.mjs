// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// UberSDR-IQ decoder for JavaScript.
//
// A decoder only. Browsers and Node receive these streams; they do not usually
// produce them, and the encoder needs a 64-bit accumulator that the decoder can
// do without. For the encoder see the Go, Python or C++ implementations.
//
// NO BigInt IS NEEDED, AND THAT IS NOT AN ACCIDENT
//
// The format clamps every filter tap to 2^24 and samples are 16-bit, so each
// product stays under 2^40 and an order-16 sum under 2^45. A float64 holds
// integers exactly to 2^53, so plain numbers here are exact rather than
// approximately right. The tap clamp exists for int64 headroom in the reference
// implementation; it is what makes this port clean.
//
// The 64-bit timestamp is the one field that would need BigInt. It is returned
// as two 32-bit halves so that a caller who needs it can assemble it however it
// likes, and ignored otherwise.
//
// EVERYTHING ELSE IS LOAD-BEARING. The predictor is backward adaptive, so this
// side recomputes the taps rather than being told them; the rounding of the
// prediction sum, the sign convention, the tap clamp and the order in which
// cascade stages are undone must all match the encoder exactly. A difference in
// any of them does not fail loudly, it returns plausible-sounding noise.
//
// A decoder IS a stream. Construct one per stream, feed it every packet in
// order starting from the first, and call reset() if the stream restarts. A
// packet that is skipped -- even one whose samples you intend to discard --
// leaves these filters where the encoder's no longer are.

// "PCM4" little-endian. Four bytes and not two because these packets may share
// a transport with frames that carry no magic, and the width of a magic is a
// false-positive rate.
export const MAGIC = 0x344d4350;

export const FLAG_ESCAPE = 1 << 7;
export const FLAG_QUALITY = 1 << 6;
export const FLAG_METADATA = 1 << 5;
export const FLAG_SILENT = 1 << 4;
export const FLAG_COUNT = 1 << 3;
export const PROFILE_MASK = 0x07;

const TAP_SCALE = 65536; // Q16
const TAP_LIMIT = 1 << 24;
const QUALITY_NO_READING = -32768;

// Tap leakage divisors: 2^14 for a complex stage, 2^16 for a real one. These
// are divisors rather than shift counts on purpose -- see leak() below.
const LEAK_DIV_COMPLEX = 16384;
const LEAK_DIV_REAL = 65536;

// The largest shift a scaled (profile 2) body may carry.
const MAX_SHIFT = 15;

// Predictor profiles. The packet declares which one it was coded with, so this
// decoder never infers it from the sample rate or the channel count: the sender
// decides and can be retuned without a client change.
//
// The two differ because the signals do. Complex baseband has a carrier that is
// a single complex pole, which one complex tap cancels; treating I and Q as two
// real streams throws that away. Demodulated audio is mono, so the same filter
// with the imaginary terms dropped -- and it wants a deep cascade of short
// filters, because an oversampled channel has structure at several scales.
//
// Profile 2 is profile 0's predictor over requantised samples: the filters run
// on the values that are on the wire, exactly as for profile 0, and the shift
// carried in the body is undone only on the way out. It is a separate id and
// not a flag so that a decoder which does not implement the restore refuses the
// stream instead of playing it 6*shift dB low.
const PROFILES = {
    0: { name: 'iq-complex-o16', complex: true, orders: [16], mus: [16] },
    1: { name: 'audio-real-8/8/4/2', complex: false, orders: [8, 8, 4, 2], mus: [16, 16, 32, 32] },
    2: { name: 'iq-complex-o16-scaled', complex: true, orders: [16], mus: [16], scaled: true },
};

// Round-to-nearest shift by 16, away from zero on ties. A plain >> rounds
// negatives towards negative infinity and would diverge from the encoder.
function rshift16(v) {
    return v >= 0
        ? Math.floor((v + 32768) / TAP_SCALE)
        : -Math.floor((-v + 32768) / TAP_SCALE);
}

// The tap leak: the magnitude of the tap divided by 2^s, truncated towards
// zero, with the sign put back. Subtracted from every tap on every update that
// runs, which is what stops the taps walking; without it a carrier-dense band
// drives them into the clamp in a minute and a half.
//
// THIS IS A DIVISION AND NOT A SHIFT, DELIBERATELY. JavaScript's >> is a 32-bit
// operator and taps reach past 2^31, so `w >> 14` would wrap and silently
// corrupt the filter state. Taps are exact integers in a float64, so dividing
// the magnitude and flooring is the exact truncation the format specifies.
function leak(w, div) {
    return w < 0 ? -Math.floor(-w / div) : Math.floor(w / div);
}

function sgn(v) { return v > 0 ? 1 : v < 0 ? -1 : 0; }

function clampTap(w) {
    return w > TAP_LIMIT ? TAP_LIMIT : w < -TAP_LIMIT ? -TAP_LIMIT : w;
}

// One adaptive filter. Complex and real are the same algorithm -- the real form
// is the complex one with the imaginary terms removed -- so both live here
// rather than in two near-identical classes.
class Stage {
    constructor(order, mu, complex) {
        this.order = order;
        this.mu = mu;
        this.complex = complex;
        this.wr = new Float64Array(order);
        this.hr = new Float64Array(order * 2);
        this.sr = new Float64Array(order * 2);
        this.leakDiv = complex ? LEAK_DIV_COMPLEX : LEAK_DIV_REAL;
        if (complex) {
            this.wi = new Float64Array(order);
            this.hi = new Float64Array(order * 2);
            this.si = new Float64Array(order * 2);
        }
        this.idx = order;
    }

    // Reconstruct one sample from its residual, then adapt and record it. The
    // encoder performs the identical prediction and adaptation on its side,
    // which is what keeps the two in step with nothing transmitted.
    stepReal(e) {
        const { wr, hr, order } = this;
        const base = this.idx - 1;
        let p = 0;
        for (let j = 0; j < order; j++) p += wr[j] * hr[base - j];
        const x = e + rshift16(p);
        this._adapt1(e);
        this._push1(x);
        return x;
    }

    // The tap update. Leaky sign-sign LMS: each tap moves by mu in the
    // direction that would have reduced this error, less a fraction of its own
    // old value, the whole thing clamped.
    //
    // The early return on a zero error is REQUIRED and not an optimisation.
    // Prediction over an all-zero history is zero, so the error over digital
    // silence is zero, and this branch is what freezes the taps across a
    // squelched run instead of leaking them towards zero. Adding the leak but
    // running it on a zero error parts company with the encoder on the first
    // silent packet.
    _adapt1(e) {
        if (e === 0) return;
        const { wr, sr, order } = this;
        const base = this.idx - 1;
        const div = this.leakDiv;
        const m = this.mu * sgn(e);
        for (let j = 0; j < order; j++) {
            const w = wr[j];
            wr[j] = clampTap(w + m * sr[base - j] - leak(w, div));
        }
    }

    // The complex form. The pair adapts jointly: the update is skipped only
    // when both components of the error are zero, so a frame with a zero I and
    // a non-zero Q still adapts and still leaks. Each tap leaks from its own
    // old value -- wr from wr and wi from wi, not from the magnitude of the
    // pair -- and the leak comes off in the same expression as the gradient
    // step, before the clamp.
    _adapt2(er, ei) {
        if (er === 0 && ei === 0) return;
        const { wr, wi, sr, si, order } = this;
        const base = this.idx - 1;
        const div = this.leakDiv;
        const mr = this.mu * sgn(er), mi = this.mu * sgn(ei);
        for (let j = 0; j < order; j++) {
            const hrs = sr[base - j];
            const his = -si[base - j]; // the conjugate the complex LMS gradient needs
            const a = wr[j], b = wi[j];
            wr[j] = clampTap(a + mr * hrs - mi * his - leak(a, div));
            wi[j] = clampTap(b + mr * his + mi * hrs - leak(b, div));
        }
    }

    stepComplex(er, ei, out) {
        const { wr, wi, hr, hi, order } = this;
        const base = this.idx - 1;
        let pr = 0, pi = 0;
        for (let j = 0; j < order; j++) {
            const br = hr[base - j], bi = hi[base - j];
            pr += wr[j] * br - wi[j] * bi;
            pi += wr[j] * bi + wi[j] * br;
        }
        const xr = er + rshift16(pr);
        const xi = ei + rshift16(pi);
        this._adapt2(er, ei);
        this._push2(xr, xi);
        out[0] = xr;
        out[1] = xi;
    }

    // The encoder direction, needed where the samples are already known: the
    // verbatim escape and the silent mode both send no residuals but still
    // require both sides to advance their filters over the samples.
    advanceReal(x) {
        const { wr, hr, order } = this;
        const base = this.idx - 1;
        let p = 0;
        for (let j = 0; j < order; j++) p += wr[j] * hr[base - j];
        const e = x - rshift16(p);
        this._adapt1(e);
        this._push1(x);
        return e;
    }

    advanceComplex(xr, xi, out) {
        const { wr, wi, hr, hi, order } = this;
        const base = this.idx - 1;
        let pr = 0, pi = 0;
        for (let j = 0; j < order; j++) {
            const br = hr[base - j], bi = hi[base - j];
            pr += wr[j] * br - wi[j] * bi;
            pi += wr[j] * bi + wi[j] * br;
        }
        const er = xr - rshift16(pr);
        const ei = xi - rshift16(pi);
        this._adapt2(er, ei);
        this._push2(xr, xi);
        out[0] = er;
        out[1] = ei;
    }

    // History is linear with a periodic slide rather than circular, so the tap
    // loops walk contiguous memory with no index wrapping. This runs per sample.
    _push1(x) {
        this.hr[this.idx] = x;
        this.sr[this.idx] = sgn(x);
        if (++this.idx === this.hr.length) {
            const n = this.order;
            this.hr.copyWithin(0, n, n * 2);
            this.sr.copyWithin(0, n, n * 2);
            this.idx = n;
        }
    }

    _push2(xr, xi) {
        this.hr[this.idx] = xr;
        this.hi[this.idx] = xi;
        this.sr[this.idx] = sgn(xr);
        this.si[this.idx] = sgn(xi);
        if (++this.idx === this.hr.length) {
            const n = this.order;
            this.hr.copyWithin(0, n, n * 2);
            this.hi.copyWithin(0, n, n * 2);
            this.sr.copyWithin(0, n, n * 2);
            this.si.copyWithin(0, n, n * 2);
            this.idx = n;
        }
    }
}

// Trailing-zero count per byte, for the Rice unary runs.
const CTZ = new Uint8Array(256);
for (let i = 1; i < 256; i++) {
    let n = 0, v = i;
    while ((v & 1) === 0) { v >>= 1; n++; }
    CTZ[i] = n;
}

// Reads the Rice bitstream: per value, a run of 1 bits, a 0 stop bit, then k
// raw low bits, zigzagged. Bits are least-significant first within each byte.
function riceDecode(body, count, out) {
    const k = body[0];
    if (k > 30) return false;
    let bytePos = 1, acc = 0, nbits = 0;
    const len = body.length;
    const kMul = Math.pow(2, k);

    for (let i = 0; i < count; i++) {
        let q = 0;
        for (;;) {
            if (nbits === 0) {
                if (bytePos >= len) return false;
                acc = body[bytePos++];
                nbits = 8;
            }
            const inv = ~acc & 0xff;
            const t = inv === 0 ? nbits : Math.min(CTZ[inv], nbits);
            q += t;
            if (t < nbits) {
                acc >>= t + 1;
                nbits -= t + 1;
                break;
            }
            acc = 0;
            nbits = 0;
        }
        let rem = 0, need = k, shift = 0;
        while (need > 0) {
            if (nbits === 0) {
                if (bytePos >= len) return false;
                acc = body[bytePos++];
                nbits = 8;
            }
            const take = need < nbits ? need : nbits;
            rem += (acc & ((1 << take) - 1)) * (1 << shift);
            acc >>= take;
            nbits -= take;
            shift += take;
            need -= take;
        }
        const u = q * kMul + rem;
        out[i] = (u % 2 === 1) ? -(u + 1) / 2 : u / 2;
    }
    return true;
}

// Signed centidecibels back to dB, with one codepoint reserved for "no
// reading" -- the -999 sentinel used at the API level cannot be represented,
// since -99900 overflows an int16.
function quality(v) {
    return v === QUALITY_NO_READING ? null : v / 100;
}

function uvarint(u8, off) {
    let x = 0, s = 0;
    for (let i = off; i < u8.length && i < off + 10; i++) {
        const b = u8[i];
        if (b < 0x80) return [x + b * Math.pow(2, s), i + 1];
        x += (b & 0x7f) * Math.pow(2, s);
        s += 7;
    }
    return null;
}

// Length of the varint at off, without decoding it: used to step over the
// timestamp delta.
function varintLen(u8, off) {
    for (let i = off; i < u8.length && i < off + 10; i++) {
        if (u8[i] < 0x80) return i - off + 1;
    }
    return -1;
}

// True when a binary frame carries an UberSDR-IQ header. Useful where these
// share a transport with frames of another kind.
export function isPacket(buffer) {
    const u8 = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
    if (u8.length < 5) return false;
    return (u8[0] | (u8[1] << 8) | (u8[2] << 16) | (u8[3] << 24)) >>> 0 === MAGIC;
}

export class StreamDecoder {
    constructor() { this.reset(); }

    // Drops the adaptation and every carried-forward field. Call this when a
    // stream restarts, because the encoder starts a fresh one too.
    reset() {
        this.stages = null;
        this.profileId = -1;
        this.complex = false;
        this.scaled = false;
        this.sampleRate = 0;
        this.channels = 1;
        this.sampleCount = 0;
        this.power = QUALITY_NO_READING;
        this.noise = QUALITY_NO_READING;
        this.haveMetadata = false;
        this.residuals = null;
        this.pair = new Float64Array(2);
    }

    _useProfile(id) {
        if (this.profileId === id && this.stages) return true;
        const p = PROFILES[id];
        // An unimplemented profile is refused rather than guessed at: decoding
        // with the wrong predictor returns plausible noise instead of failing,
        // which is the worst outcome for a format whose promise is exactness.
        if (!p) return false;
        this.stages = p.orders.map((o, i) => new Stage(o, p.mus[i], p.complex));
        this.complex = p.complex;
        this.scaled = p.scaled === true;
        this.profileId = id;
        return true;
    }

    // Decode one packet.
    //
    // Returns null for a frame that cannot be read, including one that arrives
    // before any resynchronisation point -- which is what joining a stream late
    // looks like. Otherwise:
    //
    //   samples       Int16Array, interleaved I/Q when channels is 2
    //   sampleRate, channels
    //   signal        { basebandPower, noise } in dB, or null for no reading
    //   timestampLow, timestampHigh   the 64-bit timestamp in two halves
    //   silent, escape, profile
    //   shift         the reduced-depth shift already applied to samples on
    //                 profile 2, and 0 on every other profile
    decode(buffer) {
        const u8 = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
        if (u8.length < 5) return null;
        const view = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
        if (view.getUint32(0, true) !== MAGIC) return null;

        const flags = u8[4];
        let off = 5;
        const escape = (flags & FLAG_ESCAPE) !== 0;
        const silent = (flags & FLAG_SILENT) !== 0;
        if (escape && silent) return null;
        if (!this._useProfile(flags & PROFILE_MASK)) return null;

        // A resynchronisation carries the full 64-bit timestamp; every other
        // packet carries a delta, which this decoder steps over rather than
        // accumulating -- see the note on BigInt at the top.
        let tsLow = this.tsLow || 0, tsHigh = this.tsHigh || 0;
        if (flags & FLAG_METADATA) {
            if (off + 8 > u8.length) return null;
            tsLow = view.getUint32(off, true);
            tsHigh = view.getUint32(off + 4, true);
            off += 8;
        } else {
            if (!this.haveMetadata) return null; // joined late: wait for a resync
            const n = varintLen(u8, off);
            if (n < 0) return null;
            off += n;
        }
        this.tsLow = tsLow;
        this.tsHigh = tsHigh;

        if (flags & FLAG_COUNT) {
            const r = uvarint(u8, off);
            if (!r) return null;
            this.sampleCount = r[0];
            off = r[1];
        }
        if (flags & FLAG_METADATA) {
            const r = uvarint(u8, off);
            if (!r) return null;
            this.sampleRate = r[0];
            off = r[1];
            if (off >= u8.length) return null;
            this.channels = u8[off++] || 1;
            this.haveMetadata = true;
        } else if (!this.haveMetadata) {
            return null;
        }
        if (flags & FLAG_QUALITY) {
            if (off + 4 > u8.length) return null;
            this.power = view.getInt16(off, true);
            this.noise = view.getInt16(off + 2, true);
            off += 4;
        }
        if (!this.sampleRate || this.sampleCount <= 0) return null;

        const count = this.sampleCount;
        const step = this.complex ? 2 : 1;
        if (count % step !== 0) return null;
        const samples = new Int16Array(count);
        let shift = 0;

        if (silent) {
            // No body was sent. The samples are zero, but the predictor still
            // has to run: the encoder advanced its filters across this packet,
            // and everything after it decodes wrongly if this side does not.
            //
            // A silent packet carries no shift byte whatever the profile,
            // because it carries no body at all. The silent flag is therefore
            // tested before any attempt to read one.
            if (off !== u8.length) return null;
            this._advanceZeros(count);
            return this._result(samples, silent, escape, shift, tsLow, tsHigh);
        }

        // Profile 2 prefixes every body it sends -- coded or escaped -- with
        // the shift its samples were requantised by. The predictor runs on the
        // requantised values; the scale comes off only on the way out.
        if (this.scaled) {
            if (off >= u8.length) return null;
            shift = u8[off++];
            if (shift > MAX_SHIFT) return null;
        }

        if (escape) {
            if (off + count * 2 > u8.length) return null;
            for (let i = 0; i < count; i += step) {
                const a = view.getInt16(off + 2 * i, true);
                if (step === 2) {
                    const b = view.getInt16(off + 2 * i + 2, true);
                    this._advance2(a, b);
                    samples[i] = a;
                    samples[i + 1] = b;
                } else {
                    this._advance1(a);
                    samples[i] = a;
                }
            }
        } else {
            const body = u8.subarray(off);
            if (body.length < 1) return null;
            if (!this.residuals || this.residuals.length < count) {
                this.residuals = new Float64Array(count);
            }
            if (!riceDecode(body, count, this.residuals)) return null;
            const res = this.residuals;
            if (step === 2) {
                for (let i = 0; i < count; i += 2) {
                    const [xr, xi] = this._step2(res[i], res[i + 1]);
                    samples[i] = xr;
                    samples[i + 1] = xi;
                }
            } else {
                for (let i = 0; i < count; i++) samples[i] = this._step1(res[i]);
            }
        }

        // The restore, and only here: after the filters have run. Saturating
        // rather than wrapping, because rounding in the sender's quantiser can
        // carry a full-scale peak one step past the ceiling and a wrap would
        // turn it into the opposite rail.
        if (shift !== 0) {
            for (let i = 0; i < count; i++) {
                const r = samples[i] << shift;
                samples[i] = r > 32767 ? 32767 : r < -32768 ? -32768 : r;
            }
        }

        return this._result(samples, silent, escape, shift, tsLow, tsHigh);
    }

    _result(samples, silent, escape, shift, tsLow, tsHigh) {
        return {
            samples,
            sampleRate: this.sampleRate,
            channels: this.channels,
            profile: this.profileId,
            silent,
            escape,
            shift,
            timestampLow: tsLow,
            timestampHigh: tsHigh,
            signal: {
                basebandPower: quality(this.power),
                noise: quality(this.noise),
            },
        };
    }

    // Stages are undone in reverse: the last to have predicted is the first to
    // be reversed.
    _step1(e) {
        const st = this.stages;
        let v = e;
        for (let i = st.length - 1; i >= 0; i--) v = st[i].stepReal(v);
        return v;
    }

    _step2(er, ei) {
        const st = this.stages;
        let a = er, b = ei;
        const pair = this.pair;
        for (let i = st.length - 1; i >= 0; i--) {
            st[i].stepComplex(a, b, pair);
            a = pair[0];
            b = pair[1];
        }
        return [a, b];
    }

    // Forward direction, for packets whose samples arrive without residuals.
    _advance1(x) {
        const st = this.stages;
        let v = x;
        for (let i = 0; i < st.length; i++) v = st[i].advanceReal(v);
    }

    _advance2(xr, xi) {
        const st = this.stages;
        let a = xr, b = xi;
        const pair = this.pair;
        for (let i = 0; i < st.length; i++) {
            st[i].advanceComplex(a, b, pair);
            a = pair[0];
            b = pair[1];
        }
    }

    _advanceZeros(count) {
        if (this.complex) {
            for (let i = 0; i < count; i += 2) this._advance2(0, 0);
        } else {
            for (let i = 0; i < count; i++) this._advance1(0);
        }
    }
}

// Reads the length-prefixed stream container: [length uint32 LE][packet]...
// The container is a convenience for stored streams, not part of the format.
export function* readStream(bytes) {
    const u8 = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
    const view = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
    let off = 0;
    while (off + 4 <= u8.length) {
        const n = view.getUint32(off, true);
        off += 4;
        if (n === 0 || off + n > u8.length) throw new Error('truncated stream file');
        yield u8.subarray(off, off + n);
        off += n;
    }
}
