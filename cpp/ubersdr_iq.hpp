// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// UberSDR-IQ: lossless compression for SDR sample streams.
//
// A complete reference implementation -- encoder and decoder -- in a single
// header. C++17, no dependencies beyond the standard library, no exceptions:
// every operation that can fail returns false and writes a reason into a
// std::string.
//
// One profile is the exception to "lossless" and is labelled as such on every
// packet that uses it: kProfileIQScaled requantises IQ samples by a power of
// two for a client that has asked for a reduced depth. The codec is still exact
// over what it is given, and the scale travels in the packet.
//
// The format predicts each 16-bit sample from the ones before it with an
// adaptive filter and Rice-codes the prediction error. The filter is BACKWARD
// adaptive: its taps are derived from samples that have already been coded, so
// the other end recomputes them independently and nothing about them is
// transmitted.
//
// That is what makes every arithmetic detail here load-bearing. The two ends
// never exchange filter state; they agree only by computing the same thing.
// The rounding of the prediction sum, the sign convention, the tap leak and
// the skip of it on a zero error, the tap clamp, the order in which cascade
// stages are applied and the point at which the fast path skips the clamp must
// all match exactly. A difference in any of them does not fail loudly -- it
// returns plausible-sounding noise.
//
// The arithmetic below is written to be exactly the reference implementation's
// on every compiler rather than merely usually: negation goes through unsigned
// so the most negative value cannot overflow, and no result depends on how a
// negative value is shifted right, which C++17 leaves implementation-defined.
//
// A codec instance IS a stream. It carries the adaptation of every sample seen
// so far, so create one per stream, use it from one thread, and drop it when
// the stream ends. Every packet must pass through it, in order, from the
// first: a packet that is skipped leaves this side's filters where the other
// end's no longer are.
//
// See SPEC.md for the format itself.

#ifndef UBERSDR_IQ_HPP
#define UBERSDR_IQ_HPP

#include <algorithm>
#include <cstdint>
#include <cstring>
#include <istream>
#include <ostream>
#include <string>
#include <vector>

namespace ubersdr_iq {

// ---------------------------------------------------------------------------
// Wire constants
// ---------------------------------------------------------------------------

// Identifies a packet header. Little-endian on the wire it reads "PCM4", which
// makes it findable in a hex dump; the string is retained from the format's
// original name and carries no meaning of its own. Four bytes and not one or
// two because these packets may share a transport with frames that carry no
// magic of their own, and the width of a magic is a false-positive rate.
constexpr uint32_t kMagic = 0x344D4350u;

constexpr uint8_t kFlagEscape   = 0x80;
constexpr uint8_t kFlagQuality  = 0x40;
constexpr uint8_t kFlagMetadata = 0x20;
constexpr uint8_t kFlagSilent   = 0x10;
constexpr uint8_t kFlagCount    = 0x08;
constexpr uint8_t kProfileMask  = 0x07;

// The codepoint for "no reading". It stands in for the -999 sentinel used at
// the API level, which cannot be represented in centidecibels because -99900
// overflows an int16.
constexpr int16_t kQualityNoReading = INT16_MIN;

// How often metadata is retransmitted regardless of change, in nanoseconds.
constexpr uint64_t kDefaultResyncNanos = 5000000000ull;

// Profile ids. Part of the wire format; never reassigned.
constexpr uint8_t kProfileIQ    = 0;  // one complex filter, order 16
constexpr uint8_t kProfileAudio = 1;  // four real stages, orders 8/8/4/2
// kProfileIQScaled is kProfileIQ over requantised samples: the same complex
// filter of order 16, with a power-of-two shift applied to the samples before
// they reach the encoder's predictor and undone after they leave the decoder's.
// The shift is the first byte of the body, and a silent packet -- which has no
// body -- carries no shift at all.
//
// It is a separate profile id rather than a flag bit precisely so a decoder
// that never asked for a reduced depth cannot be handed one by accident: an
// unknown profile is a hard error everywhere, where an unrecognised flag bit
// might be ignored, and a stream silently rendered 6*shift dB low is worse than
// one that fails.
constexpr uint8_t kProfileIQScaled = 2;  // as kProfileIQ, over requantised samples

// The largest shift a scaled body may carry. Sixteen would leave nothing of an
// int16, and the reference sender's depth selector never goes past 15, so
// anything above this is a corrupt or hostile packet rather than a stream to
// try to render.
constexpr unsigned kMaxShift = 15;

constexpr int kMaxStages = 4;

// ---------------------------------------------------------------------------
// Signal quality
// ---------------------------------------------------------------------------

// A dB reading as the signed centidecibels the header carries. 0.01 dB keeps
// the error in a derived SNR below 0.01 dB, far under what any receiver
// displays. The -999 sentinel, and any non-finite value, become
// kQualityNoReading -- a reading that is not a number is not a reading.
inline int16_t QualityFromFloat(float v) {
    double d = static_cast<double>(v);
    if (!(d > -998.0)) return kQualityNoReading;  // also catches NaN and -inf
    if (d < -327.67) d = -327.67;
    if (d > 327.67) d = 327.67;
    double x = d * 100.0;
    return static_cast<int16_t>(x >= 0 ? static_cast<int64_t>(x + 0.5)
                                       : -static_cast<int64_t>(-x + 0.5));
}

inline float QualityToFloat(int16_t q) {
    if (q == kQualityNoReading) return -999.0f;
    return static_cast<float>(q / 100.0);
}

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

// One packet's metadata, in the terms callers use. Which fields were actually
// transmitted is the header coder's business; every field is filled in on
// every packet, carried forward from the last resynchronisation point when the
// packet did not repeat it.
struct Header {
    // Time of the first sample, nanoseconds since the epoch.
    uint64_t timestamp_nanos = 0;
    int sample_rate = 0;
    // 1 for demodulated audio, 2 for interleaved I/Q.
    int channels = 0;
    // int16 samples in the body, counting both channels of an IQ frame. A
    // coded body has no length-to-count relationship, so this is what tells
    // the decoder when to stop.
    int sample_count = 0;
    // dBFS, or -999 for "no reading". Passengers: nothing in the codec reads
    // them.
    float baseband_power = -999.0f;
    float noise = -999.0f;
    uint8_t profile = 0;
    // The body holds verbatim little-endian int16 samples.
    bool escape = false;
    // Every sample is zero and no body was transmitted. Mutually exclusive
    // with escape.
    bool silent = false;
};

namespace detail {

inline void PutU16(std::vector<uint8_t>& out, uint16_t v) {
    out.push_back(static_cast<uint8_t>(v));
    out.push_back(static_cast<uint8_t>(v >> 8));
}

inline void PutU32(std::vector<uint8_t>& out, uint32_t v) {
    for (int i = 0; i < 4; i++) out.push_back(static_cast<uint8_t>(v >> (8 * i)));
}

inline void PutU64(std::vector<uint8_t>& out, uint64_t v) {
    for (int i = 0; i < 8; i++) out.push_back(static_cast<uint8_t>(v >> (8 * i)));
}

// Unsigned varint, seven bits per byte, low group first.
inline void PutUvarint(std::vector<uint8_t>& out, uint64_t x) {
    while (x >= 0x80) {
        out.push_back(static_cast<uint8_t>(x) | 0x80);
        x >>= 7;
    }
    out.push_back(static_cast<uint8_t>(x));
}

// Signed varint: zigzagged, then written unsigned, so a small negative delta
// still costs one byte.
inline void PutVarint(std::vector<uint8_t>& out, int64_t x) {
    uint64_t ux = static_cast<uint64_t>(x) << 1;
    if (x < 0) ux = ~ux;
    PutUvarint(out, ux);
}

inline bool ReadUvarint(const uint8_t* p, size_t len, size_t* off, uint64_t* out) {
    uint64_t x = 0;
    unsigned shift = 0;
    for (int n = 0; n < 10; n++) {
        if (*off >= len) return false;
        const uint8_t b = p[(*off)++];
        if (b < 0x80) {
            if (n == 9 && b > 1) return false;  // would overflow 64 bits
            *out = x | (static_cast<uint64_t>(b) << shift);
            return true;
        }
        x |= static_cast<uint64_t>(b & 0x7f) << shift;
        shift += 7;
    }
    return false;
}

inline bool ReadVarint(const uint8_t* p, size_t len, size_t* off, int64_t* out) {
    uint64_t ux;
    if (!ReadUvarint(p, len, off, &ux)) return false;
    *out = static_cast<int64_t>((ux >> 1) ^ (0ull - (ux & 1)));
    return true;
}

inline uint32_t ReadU32(const uint8_t* p) {
    return static_cast<uint32_t>(p[0]) | (static_cast<uint32_t>(p[1]) << 8) |
           (static_cast<uint32_t>(p[2]) << 16) | (static_cast<uint32_t>(p[3]) << 24);
}

inline uint64_t ReadU64(const uint8_t* p) {
    uint64_t v = 0;
    for (int i = 0; i < 8; i++) v |= static_cast<uint64_t>(p[i]) << (8 * i);
    return v;
}

inline int16_t ReadI16(const uint8_t* p) {
    return static_cast<int16_t>(static_cast<uint16_t>(p[0]) |
                                (static_cast<uint16_t>(p[1]) << 8));
}

inline bool Fail(std::string* err, const char* msg) {
    if (err) *err = msg;
    return false;
}

}  // namespace detail

// Writes headers for one stream.
//
// Stateful: it tracks what the peer has already been told and transmits only
// what has changed. Measured on live traffic this averages 7.7 bytes on IQ and
// 9.1 on audio, against the 37 a fixed record would cost.
class HeaderEncoder {
public:
    explicit HeaderEncoder(uint64_t resync_nanos = kDefaultResyncNanos)
        : resync_nanos_(resync_nanos) {}

    void Append(std::vector<uint8_t>& out, const Header& h) {
        const int16_t power = QualityFromFloat(h.baseband_power);
        const int16_t noise = QualityFromFloat(h.noise);
        const bool resync = NeedsResync(h);

        uint8_t flags = h.profile & kProfileMask;
        if (h.escape) flags |= kFlagEscape;
        if (h.silent) flags |= kFlagSilent;
        if (resync) flags |= kFlagMetadata;
        // Quality rides every resynchronisation so such a packet is completely
        // self-describing, and otherwise only when it has changed.
        const bool send_quality = resync || power != power_ || noise != noise_;
        if (send_quality) flags |= kFlagQuality;
        const bool send_count = resync || h.sample_count != count_;
        if (send_count) flags |= kFlagCount;

        detail::PutU32(out, kMagic);
        out.push_back(flags);
        if (resync) {
            detail::PutU64(out, h.timestamp_nanos);
        } else {
            detail::PutVarint(out, static_cast<int64_t>(h.timestamp_nanos) -
                                       static_cast<int64_t>(last_ts_));
        }
        if (send_count) detail::PutUvarint(out, static_cast<uint64_t>(h.sample_count));
        if (resync) {
            detail::PutUvarint(out, static_cast<uint64_t>(h.sample_rate));
            out.push_back(static_cast<uint8_t>(h.channels));
        }
        if (send_quality) {
            detail::PutU16(out, static_cast<uint16_t>(power));
            detail::PutU16(out, static_cast<uint16_t>(noise));
        }

        started_ = true;
        last_ts_ = h.timestamp_nanos;
        rate_ = h.sample_rate;
        channels_ = h.channels;
        power_ = power;
        noise_ = noise;
        count_ = h.sample_count;
        if (resync) last_resync_ = h.timestamp_nanos;
    }

private:
    bool NeedsResync(const Header& h) const {
        if (!started_ || h.sample_rate != rate_ || h.channels != channels_) return true;
        // A timestamp that moves backwards, or jumps further than the resync
        // interval, means the stream is not continuous where a delta would
        // assume it was.
        if (h.timestamp_nanos < last_ts_) return true;
        if (h.timestamp_nanos - last_ts_ > resync_nanos_) return true;
        return h.timestamp_nanos >= last_resync_ &&
               h.timestamp_nanos - last_resync_ >= resync_nanos_;
    }

    uint64_t resync_nanos_;
    bool started_ = false;
    uint64_t last_ts_ = 0;
    uint64_t last_resync_ = 0;
    int rate_ = 0, channels_ = 0, count_ = 0;
    int16_t power_ = 0, noise_ = 0;
};

// Reads headers for one stream, carrying forward whatever the encoder chose
// not to repeat.
class HeaderDecoder {
public:
    // Parses the header at the front of pkt, returning it and the offset at
    // which the body begins.
    //
    // A packet arriving before any metadata has been seen is rejected rather
    // than guessed at: that is the normal state for a reader that opened a
    // stored stream at an arbitrary offset, and the periodic resynchronisation
    // ends it.
    bool Decode(const uint8_t* pkt, size_t len, Header* h, size_t* body_off,
                std::string* err) {
        if (len < 5) return detail::Fail(err, "header: packet too short");
        if (detail::ReadU32(pkt) != kMagic) return detail::Fail(err, "header: bad magic");

        const uint8_t flags = pkt[4];
        size_t off = 5;
        h->profile = flags & kProfileMask;
        h->escape = (flags & kFlagEscape) != 0;
        h->silent = (flags & kFlagSilent) != 0;
        if (h->escape && h->silent)
            return detail::Fail(err, "header: escape and silent are mutually exclusive");

        if (flags & kFlagMetadata) {
            if (len < off + 8) return detail::Fail(err, "header: truncated timestamp");
            last_ts_ = detail::ReadU64(pkt + off);
            off += 8;
        } else {
            if (!have_metadata_)
                return detail::Fail(err, "header: delta packet before any resynchronisation point");
            int64_t delta;
            if (!detail::ReadVarint(pkt, len, &off, &delta))
                return detail::Fail(err, "header: malformed timestamp delta");
            last_ts_ = static_cast<uint64_t>(static_cast<int64_t>(last_ts_) + delta);
        }
        h->timestamp_nanos = last_ts_;

        if (flags & kFlagCount) {
            uint64_t c;
            if (!detail::ReadUvarint(pkt, len, &off, &c))
                return detail::Fail(err, "header: malformed sample count");
            count_ = static_cast<int>(c);
        }

        if (flags & kFlagMetadata) {
            uint64_t r;
            if (!detail::ReadUvarint(pkt, len, &off, &r))
                return detail::Fail(err, "header: malformed sample rate");
            if (off >= len) return detail::Fail(err, "header: truncated channel count");
            rate_ = static_cast<int>(r);
            channels_ = pkt[off++];
            have_metadata_ = true;
        } else if (!have_metadata_) {
            return detail::Fail(err, "header: payload before any metadata");
        }

        if (flags & kFlagQuality) {
            if (len < off + 4) return detail::Fail(err, "header: truncated signal quality");
            power_ = detail::ReadI16(pkt + off);
            noise_ = detail::ReadI16(pkt + off + 2);
            off += 4;
        }

        if (rate_ <= 0 || channels_ <= 0)
            return detail::Fail(err, "header: implausible metadata");
        if (count_ <= 0) return detail::Fail(err, "header: implausible sample count");

        h->sample_rate = rate_;
        h->channels = channels_;
        h->sample_count = count_;
        h->baseband_power = QualityToFloat(power_);
        h->noise = QualityToFloat(noise_);
        *body_off = off;
        return true;
    }

private:
    bool have_metadata_ = false;
    uint64_t last_ts_ = 0;
    int rate_ = 0, channels_ = 0, count_ = 0;
    int16_t power_ = kQualityNoReading, noise_ = kQualityNoReading;
};

// True when a binary frame carries an UberSDR-IQ header.
inline bool IsPacket(const uint8_t* pkt, size_t len) {
    return len >= 4 && detail::ReadU32(pkt) == kMagic;
}

// ---------------------------------------------------------------------------
// Adaptive filter stages
// ---------------------------------------------------------------------------

namespace detail {

// Fixed-point scale of the taps: integers in Q16, so 65536 is 1.0.
constexpr unsigned kTapShift = 16;

// Bounds |tap| to 2^24, a real magnitude of 256. It caps the prediction sum far
// below int64 overflow whatever the input does, and keeps every product inside
// the range where a float64 represents integers exactly, which is what lets a
// JavaScript port work without BigInt. Adaptation settles around 2^16, so this
// never fires in practice -- but it must be applied identically on both sides,
// because if it ever does fire the two must agree.
constexpr int64_t kTapLimit = static_cast<int64_t>(1) << 24;

// The leakage of the tap update: every adaptation subtracts w/2^shift from each
// tap alongside the gradient step.
//
// It is not optional and its absence is silent. Sign-sign LMS has no restoring
// force of its own -- the step is mu*sgn(e)*sgn(x) whatever the taps already
// are -- so in any direction the input does not excite, and a band whose energy
// sits in a few carriers leaves most directions unexcited, the taps are free to
// walk. Measured on a 909 kHz carrier in a 384 kHz baseband the mean tap
// magnitude grew linearly until it reached kTapLimit ninety seconds in, by
// which point a third of the packets were taking the escape. With the leak that
// stream holds 8.45 bits per sample for as long as it runs.
//
// The two shifts differ because the two filters differ: 14 is the complex
// stage's measured optimum on a wideband baseband, while the real cascade runs
// over a four-times-oversampled 12 kHz channel whose taps legitimately reach
// 1.2, and leaking it as hard throws that away. 16 rather than 17 for the real
// form because it is the one that can be tested: a tap has to pass 1.0 before
// it leaks at all, which every audio capture does within a second.
constexpr unsigned kLeakShiftComplex = 14;
constexpr unsigned kLeakShiftReal    = 16;

inline int64_t Sign(int64_t v) {
    return static_cast<int64_t>(v > 0) - static_cast<int64_t>(v < 0);
}

// Divide by 2^shift, rounding to nearest and away from zero on ties.
//
// A plain arithmetic shift rounds negative values towards negative infinity,
// which biases the predictor. Both ends must round the same way, so this is the
// single definition every implementation copies. Written through unsigned so
// that neither the negation nor the shift depends on anything the standard
// leaves implementation-defined.
inline int64_t RoundShift(int64_t v, unsigned shift) {
    const uint64_t half = 1ull << (shift - 1);
    if (v < 0) {
        const uint64_t a = 0ull - static_cast<uint64_t>(v);
        return -static_cast<int64_t>((a + half) >> shift);
    }
    return static_cast<int64_t>((static_cast<uint64_t>(v) + half) >> shift);
}

// The amount the leakage removes from one tap: the magnitude divided by
// 2^shift and truncated towards zero.
//
// Truncating rather than rounding is what keeps the leak from fighting the
// gradient at small taps. Below 2^shift it returns zero, so a tap the signal
// genuinely wants at a small value stays there instead of being pulled towards
// zero and pushed back on every sample.
//
// This must not be written as an arithmetic right shift. On a negative tap that
// floors rather than truncating, which is a different value; and C++17 leaves
// the result of shifting a negative value right implementation-defined, so the
// magnitude is divided through unsigned and the sign put back.
inline int64_t Leak(int64_t w, unsigned shift) {
    if (w < 0) {
        const uint64_t a = 0ull - static_cast<uint64_t>(w);
        return -static_cast<int64_t>(a >> shift);
    }
    return static_cast<int64_t>(static_cast<uint64_t>(w) >> shift);
}

inline int64_t ClampTap(int64_t w) {
    if (w > kTapLimit) return kTapLimit;
    if (w < -kTapLimit) return -kTapLimit;
    return w;
}

// History is linear rather than circular so the tap loops walk contiguous
// memory with no index wrapping; the cost is periodically sliding the newest
// `order` entries back to the front, which a window several times the order
// amortises away. An implementation choice: it changes nothing transmitted.
inline int HistoryLen(int order) { return std::max(order * 8, 64); }

// One adaptive complex filter, for interleaved I/Q.
//
// Sign-sign LMS rather than true NLMS: the update needs only the signs of the
// error and of the history, so there is no division and no normalisation, and
// it is exactly reproducible in integers.
class ComplexStage {
public:
    ComplexStage(int order, int64_t mu)
        : order_(order), mu_(mu), wr_(order, 0), wi_(order, 0),
          hr_(HistoryLen(order), 0), hi_(HistoryLen(order), 0),
          sr_(HistoryLen(order), 0), si_(HistoryLen(order), 0), idx_(order) {}

    // Decide once per packet whether Adapt may skip the tap clamp.
    //
    // One complex update moves a tap by at most 2*mu -- each of the two sign
    // terms contributes at most mu, and the leak only ever moves a tap towards
    // zero, so it cannot invalidate the bound -- and if every tap starts
    // further than 2*mu*steps from the limit, no update in this packet can
    // reach it and the clamp is an identity. This is the path that always runs
    // in practice; always clamping instead produces identical output.
    void BeginPacket(int steps) {
        int64_t m = 0;
        for (int64_t w : wr_) m = std::max(m, w < 0 ? -w : w);
        for (int64_t w : wi_) m = std::max(m, w < 0 ? -w : w);
        fast_ = m + 2 * mu_ * steps <= kTapLimit;
    }

    void Forward(int64_t xr, int64_t xi, int64_t* er, int64_t* ei) {
        int64_t pr, pi;
        Predict(&pr, &pi);
        *er = xr - pr;
        *ei = xi - pi;
        AdaptAndPush(*er, *ei, xr, xi);
    }

    void Inverse(int64_t er, int64_t ei, int64_t* xr, int64_t* xi) {
        int64_t pr, pi;
        Predict(&pr, &pi);
        *xr = er + pr;
        *xi = ei + pi;
        AdaptAndPush(er, ei, *xr, *xi);
    }

private:
    void Predict(int64_t* pr, int64_t* pi) const {
        const int lo = idx_ - order_;
        int64_t ar = 0, ai = 0;
        for (int j = 0; j < order_; j++) {
            const int64_t br = hr_[lo + j], bi = hi_[lo + j];
            const int64_t w = wr_[j], v = wi_[j];
            ar += w * br - v * bi;
            ai += w * bi + v * br;
        }
        *pr = RoundShift(ar, kTapShift);
        *pi = RoundShift(ai, kTapShift);
    }

    void AdaptAndPush(int64_t er, int64_t ei, int64_t xr, int64_t xi) {
        // Skipping a zero error is REQUIRED, not an optimisation, and it skips
        // the leak with everything else. Prediction over an all-zero history is
        // zero, so the error over digital silence is zero, and this is exactly
        // the branch that freezes the taps across a squelched run rather than
        // leaking them towards zero: whatever the filter had learned before the
        // silence is still there when signal returns. An implementation that
        // adds the leak but runs it on a zero error parts company with the
        // encoder on the first silent packet.
        //
        // The test is joint: a frame with a zero I and a non-zero Q still
        // adapts and still leaks.
        if (er != 0 || ei != 0) {
            const int lo = idx_ - order_;
            const int64_t mr = mu_ * Sign(er);
            const int64_t mi = mu_ * Sign(ei);
            // Each tap leaks from its OWN old value -- wr from wr, wi from wi,
            // not from the magnitude of the pair -- in the same expression as
            // the gradient step and before the clamp.
            if (fast_) {
                for (int j = 0; j < order_; j++) {
                    const int64_t hrs = sr_[lo + j];
                    // The conjugate of the history, as the complex LMS gradient
                    // requires: here that is the negated sign of the imaginary
                    // part.
                    const int64_t his = -si_[lo + j];
                    const int64_t owr = wr_[j], owi = wi_[j];
                    wr_[j] = owr + mr * hrs - mi * his - Leak(owr, kLeakShiftComplex);
                    wi_[j] = owi + mr * his + mi * hrs - Leak(owi, kLeakShiftComplex);
                }
            } else {
                for (int j = 0; j < order_; j++) {
                    const int64_t hrs = sr_[lo + j];
                    const int64_t his = -si_[lo + j];
                    const int64_t owr = wr_[j], owi = wi_[j];
                    wr_[j] = ClampTap(owr + mr * hrs - mi * his - Leak(owr, kLeakShiftComplex));
                    wi_[j] = ClampTap(owi + mr * his + mi * hrs - Leak(owi, kLeakShiftComplex));
                }
            }
        }
        hr_[idx_] = xr;
        hi_[idx_] = xi;
        sr_[idx_] = Sign(xr);
        si_[idx_] = Sign(xi);
        if (++idx_ == static_cast<int>(hr_.size())) {
            const int n = order_;
            std::copy(hr_.end() - n, hr_.end(), hr_.begin());
            std::copy(hi_.end() - n, hi_.end(), hi_.begin());
            std::copy(sr_.end() - n, sr_.end(), sr_.begin());
            std::copy(si_.end() - n, si_.end(), si_.begin());
            idx_ = n;
        }
    }

    int order_;
    int64_t mu_;
    // Taps in Q16, oldest-first: wr_[j] weighs the history sample at
    // hr_[idx-order+j], so prediction and adaptation walk taps and history
    // forward together.
    std::vector<int64_t> wr_, wi_;
    // History of reconstructed samples, with their signs beside them so the
    // update loop does not recompute a sign per tap per sample.
    std::vector<int64_t> hr_, hi_, sr_, si_;
    int idx_;
    bool fast_ = false;
};

// ComplexStage with the imaginary terms removed, for mono audio.
class RealStage {
public:
    RealStage(int order, int64_t mu)
        : order_(order), mu_(mu), w_(order, 0), h_(HistoryLen(order), 0),
          s_(HistoryLen(order), 0), idx_(order) {}

    // One real update moves a tap by at most mu, and the leak only ever moves
    // one towards zero, so the bound is mu*steps.
    void BeginPacket(int steps) {
        int64_t m = 0;
        for (int64_t w : w_) m = std::max(m, w < 0 ? -w : w);
        fast_ = m + mu_ * steps <= kTapLimit;
    }

    int64_t Forward(int64_t x) {
        const int64_t e = x - Predict();
        AdaptAndPush(e, x);
        return e;
    }

    int64_t Inverse(int64_t e) {
        const int64_t x = e + Predict();
        AdaptAndPush(e, x);
        return x;
    }

private:
    int64_t Predict() const {
        const int lo = idx_ - order_;
        int64_t p = 0;
        for (int j = 0; j < order_; j++) p += w_[j] * h_[lo + j];
        return RoundShift(p, kTapShift);
    }

    void AdaptAndPush(int64_t e, int64_t x) {
        // Required for the same reason as the complex form: this is what
        // freezes the taps over digital silence rather than leaking them
        // towards zero.
        if (e != 0) {
            const int lo = idx_ - order_;
            const int64_t m = mu_ * Sign(e);
            if (fast_) {
                for (int j = 0; j < order_; j++) {
                    const int64_t ow = w_[j];
                    w_[j] = ow + m * s_[lo + j] - Leak(ow, kLeakShiftReal);
                }
            } else {
                for (int j = 0; j < order_; j++) {
                    const int64_t ow = w_[j];
                    w_[j] = ClampTap(ow + m * s_[lo + j] - Leak(ow, kLeakShiftReal));
                }
            }
        }
        h_[idx_] = x;
        s_[idx_] = Sign(x);
        if (++idx_ == static_cast<int>(h_.size())) {
            const int n = order_;
            std::copy(h_.end() - n, h_.end(), h_.begin());
            std::copy(s_.end() - n, s_.end(), s_.begin());
            idx_ = n;
        }
    }

    int order_;
    int64_t mu_;
    std::vector<int64_t> w_, h_, s_;
    int idx_;
    bool fast_ = false;
};

// ---------------------------------------------------------------------------
// Rice coding
// ---------------------------------------------------------------------------
//
// A residual is coded as its zigzagged magnitude split at bit k: the high part
// in unary, then a 0 stop bit, then the low k bits raw. k is chosen once per
// packet from the mean magnitude, which for a Laplacian source is within a
// fraction of a bit of optimal, and sent as the first byte of the body.
//
// Bits are written least-significant first within each byte, bytes in
// increasing address order.

inline uint32_t Zigzag(int32_t v) {
    return (static_cast<uint32_t>(v) << 1) ^ static_cast<uint32_t>(v >> 31);
}

inline unsigned BitLength(uint64_t v) {
    unsigned n = 0;
    while (v) { v >>= 1; n++; }
    return n;
}

inline void RiceEncode(const std::vector<int32_t>& res, std::vector<uint8_t>& out) {
    uint64_t sum = 0;
    for (int32_t v : res) sum += Zigzag(v);
    unsigned k = 0;
    const uint64_t mean = sum / res.size();
    if (mean > 0) k = std::min(BitLength(mean) - 1, 30u);

    out.push_back(static_cast<uint8_t>(k));
    const uint32_t mask = k == 32 ? 0xffffffffu : ((1u << k) - 1u);
    uint64_t acc = 0;
    unsigned nbits = 0;
    for (int32_t v : res) {
        const uint32_t u = Zigzag(v);
        uint32_t q = u >> k;
        // The unary run, in chunks, so the accumulator cannot overflow however
        // long the run is. A strong carrier against a low noise floor produces
        // runs long enough that this matters.
        while (q > 0) {
            const unsigned c = q > 24 ? 24 : q;
            acc |= ((1ull << c) - 1) << nbits;
            nbits += c;
            while (nbits >= 8) {
                out.push_back(static_cast<uint8_t>(acc));
                acc >>= 8;
                nbits -= 8;
            }
            q -= c;
        }
        // The stop bit is the 0 left by not setting it, then k raw bits.
        acc |= static_cast<uint64_t>(u & mask) << (nbits + 1);
        nbits += k + 1;
        while (nbits >= 8) {
            out.push_back(static_cast<uint8_t>(acc));
            acc >>= 8;
            nbits -= 8;
        }
    }
    if (nbits) out.push_back(static_cast<uint8_t>(acc));
}

inline bool RiceDecode(const uint8_t* src, size_t len, int32_t* out, size_t count,
                       std::string* err) {
    if (len < 1) return Fail(err, "rice: empty bitstream");
    const unsigned k = src[0];
    if (k > 30) return Fail(err, "rice: invalid k");
    src++;
    len--;

    uint64_t acc = 0;
    unsigned nbits = 0;
    size_t i = 0;
    auto refill = [&]() {
        while (nbits <= 56 && i < len) {
            acc |= static_cast<uint64_t>(src[i]) << nbits;
            i++;
            nbits += 8;
        }
    };
    refill();
    const uint64_t mask = (1ull << k) - 1;

    for (size_t j = 0; j < count; j++) {
        if (nbits < 48) refill();
        unsigned q = 0;
        for (;;) {
            // Bits past nbits read as zero, so a unary run that reaches the end
            // of the accumulator simply continues after a refill.
            const uint64_t inv = ~acc;
            unsigned c = 0;
            if (inv == 0) {
                c = 64;
            } else {
                while ((inv >> c & 1) == 0) c++;
            }
            if (c < nbits) {
                q += c;
                // The shift is by c+1, which reaches 64 when a 63-bit unary
                // run is counted out of a full accumulator -- nbits tops out
                // at exactly 64 because refill() runs while nbits <= 56.
                //
                // Shifting a uint64_t by 64 is undefined in C++. On x86 the
                // count is taken modulo 64, so the accumulator keeps every bit
                // it had and the next refill ORs fresh bytes into stale ones;
                // the rest of the packet then decodes as noise, which the
                // backward-adaptive predictor takes into its taps, so the
                // stream stays wrong for the life of the connection. Go,
                // Python and JavaScript all yield zero here, which is what
                // this has to reproduce. See the rice-63 conformance vector.
                const unsigned shift = c + 1;
                acc = (shift >= 64) ? 0 : (acc >> shift);
                nbits -= shift;
                break;
            }
            if (i >= len) return Fail(err, "rice: truncated bitstream");
            q += nbits;
            acc = 0;
            nbits = 0;
            refill();
        }
        if (nbits < k) refill();
        if (nbits < k) return Fail(err, "rice: truncated remainder");
        const uint32_t u = (static_cast<uint32_t>(q) << k) | static_cast<uint32_t>(acc & mask);
        acc >>= k;
        nbits -= k;
        out[j] = static_cast<int32_t>((u >> 1) ^ (0u - (u & 1)));
    }
    return true;
}

}  // namespace detail

// ---------------------------------------------------------------------------
// Payload codec
// ---------------------------------------------------------------------------

// Codes one direction of one stream's payloads.
//
// Stateful across packets and not safe for concurrent use: create one per
// stream, call it from one thread, and drop it when the stream ends.
class Codec {
public:
    // Build a codec for a profile id, rejecting one this build does not
    // implement.
    //
    // The failure is deliberate. Falling back to a default profile would decode
    // with the wrong predictor and return plausible-sounding noise rather than
    // failing, which is the worst outcome for a format whose promise is
    // bit-exactness.
    static bool Create(uint8_t profile_id, Codec* out, std::string* err) {
        switch (profile_id) {
            case kProfileIQ:
                out->Init(profile_id, true, {16}, {16}, false);
                return true;
            case kProfileAudio:
                out->Init(profile_id, false, {8, 8, 4, 2}, {16, 16, 32, 32}, false);
                return true;
            case kProfileIQScaled:
                // Profile 0's predictor exactly, over requantised samples. The
                // filters run on the values that are on the wire, on both
                // sides; only the body layout and the output scale differ.
                out->Init(profile_id, true, {16}, {16}, true);
                return true;
            default:
                return detail::Fail(err, "unknown profile id");
        }
    }

    uint8_t profile() const { return profile_; }
    const char* profile_name() const {
        switch (profile_) {
            case kProfileIQ: return "iq-complex-o16";
            case kProfileAudio: return "audio-real-8/8/4/2";
            case kProfileIQScaled: return "iq-complex-o16-scaled";
            default: return "unknown";
        }
    }
    // True when the body carries a leading shift byte and the decoded samples
    // are scaled back up by it on the way out. It changes nothing about the
    // predictor.
    bool scaled() const { return scaled_; }
    // 2 for a complex profile, which consumes interleaved I/Q frames.
    int samples_per_step() const { return complex_ ? 2 : 1; }

    // Code one packet. body receives the payload; *escape reports that it holds
    // verbatim little-endian int16 samples rather than a coded bitstream. The
    // caller must carry that bit, and the profile id, in the packet header.
    bool Encode(const int16_t* samples, size_t n, std::vector<uint8_t>* body,
                bool* escape, std::string* err) {
        const int step = samples_per_step();
        if (n == 0) return detail::Fail(err, "empty packet");
        if (n % static_cast<size_t>(step) != 0)
            return detail::Fail(err, "not a whole number of frames");

        BeginPacket(static_cast<int>(n) / step);
        res_.resize(n);
        for (size_t i = 0; i < n; i += step) {
            int64_t a = samples[i], b = step == 2 ? samples[i + 1] : 0;
            Forward(&a, &b);
            res_[i] = static_cast<int32_t>(a);
            if (step == 2) res_[i + 1] = static_cast<int32_t>(b);
        }

        body->clear();
        detail::RiceEncode(res_, *body);

        // If prediction and coding did not pay for themselves, send the samples
        // as they are. A predictor cannot help a full-entropy signal, and a
        // saturated front end produces exactly that; without this the format
        // would expand by about 3% on such a stream. The filters have already
        // adapted over this packet, and the decoder adapts over the same
        // verbatim samples, so state stays in step through an escape.
        if (body->size() >= n * 2) {
            body->clear();
            body->reserve(n * 2);
            for (size_t i = 0; i < n; i++) {
                const uint16_t u = static_cast<uint16_t>(samples[i]);
                body->push_back(static_cast<uint8_t>(u));
                body->push_back(static_cast<uint8_t>(u >> 8));
            }
            *escape = true;
            return true;
        }
        *escape = false;
        return true;
    }

    // Advance the filters over count zero samples without producing a body.
    //
    // A packet whose samples are all zero says so in its header and carries no
    // body. Rice coding cannot get an all-zero residual below one bit per
    // sample -- it is a bare stop bit -- so a 240-sample packet would cost 30
    // bytes of stop bits however well the predictor did. The predictor still
    // has to run: the other end advances its own filters over the same zeros,
    // and the two must agree sample for sample.
    bool AdvanceSilence(size_t count, std::string* err) {
        const int step = samples_per_step();
        if (count == 0) return detail::Fail(err, "empty packet");
        if (count % static_cast<size_t>(step) != 0)
            return detail::Fail(err, "not a whole number of frames");
        BeginPacket(static_cast<int>(count) / step);
        for (size_t i = 0; i < count; i += step) {
            int64_t a = 0, b = 0;
            Forward(&a, &b);
        }
        return true;
    }

    // Reconstruct one packet body into out, which is resized to count.
    bool Decode(const uint8_t* body, size_t len, size_t count, bool escape,
                std::vector<int16_t>* out, std::string* err) {
        const int step = samples_per_step();
        if (count == 0 || count % static_cast<size_t>(step) != 0)
            return detail::Fail(err, "bad sample count for this profile");
        out->resize(count);

        if (escape) {
            if (len < count * 2) return detail::Fail(err, "escape body truncated");
            for (size_t i = 0; i < count; i++) {
                (*out)[i] = static_cast<int16_t>(static_cast<uint16_t>(body[2 * i]) |
                                                 (static_cast<uint16_t>(body[2 * i + 1]) << 8));
            }
            // Advance the filters over these samples exactly as the encoder
            // did, discarding the residuals.
            BeginPacket(static_cast<int>(count) / step);
            for (size_t i = 0; i < count; i += step) {
                int64_t a = (*out)[i], b = step == 2 ? (*out)[i + 1] : 0;
                Forward(&a, &b);
            }
            return true;
        }

        res_.resize(count);
        if (!detail::RiceDecode(body, len, res_.data(), count, err)) return false;

        BeginPacket(static_cast<int>(count) / step);
        if (complex_) {
            for (size_t i = 0; i < count; i += 2) {
                int64_t a = res_[i], b = res_[i + 1];
                // Stages are inverted in reverse order: the last stage to have
                // predicted is the first to be undone.
                for (int j = ncx_ - 1; j >= 0; j--) {
                    int64_t xr, xi;
                    cx_[j].Inverse(a, b, &xr, &xi);
                    a = xr;
                    b = xi;
                }
                (*out)[i] = static_cast<int16_t>(a);
                (*out)[i + 1] = static_cast<int16_t>(b);
            }
        } else {
            for (size_t i = 0; i < count; i++) {
                int64_t a = res_[i];
                for (int j = nrl_ - 1; j >= 0; j--) a = rl_[j].Inverse(a);
                (*out)[i] = static_cast<int16_t>(a);
            }
        }
        return true;
    }

private:
    void Init(uint8_t id, bool is_complex, std::vector<int> orders,
              std::vector<int64_t> mus, bool is_scaled) {
        profile_ = id;
        complex_ = is_complex;
        scaled_ = is_scaled;
        cx_.clear();
        rl_.clear();
        for (size_t i = 0; i < orders.size(); i++) {
            if (is_complex) {
                cx_.emplace_back(orders[i], mus[i]);
            } else {
                rl_.emplace_back(orders[i], mus[i]);
            }
        }
        ncx_ = static_cast<int>(cx_.size());
        nrl_ = static_cast<int>(rl_.size());
    }

    void BeginPacket(int steps) {
        for (auto& s : cx_) s.BeginPacket(steps);
        for (auto& s : rl_) s.BeginPacket(steps);
    }

    // Run the cascade in the encoder direction over one sample position. Each
    // stage predicts the residual left by the one before it.
    void Forward(int64_t* a, int64_t* b) {
        if (complex_) {
            for (auto& s : cx_) {
                int64_t er, ei;
                s.Forward(*a, *b, &er, &ei);
                *a = er;
                *b = ei;
            }
            return;
        }
        for (auto& s : rl_) *a = s.Forward(*a);
        *b = 0;
    }

    uint8_t profile_ = 0xff;
    bool complex_ = false;
    bool scaled_ = false;
    std::vector<detail::ComplexStage> cx_;
    std::vector<detail::RealStage> rl_;
    int ncx_ = 0, nrl_ = 0;
    std::vector<int32_t> res_;
};

// The sender's default policy for which predictor to use. Nothing on the wire
// depends on it -- the packet declares the result -- so a sender may replace it
// freely, including per band or per mode.
// It never returns kProfileIQScaled: a reduced depth is not something a channel
// count implies, it is something a client asks for, and a sender that has been
// asked calls StreamEncoder::EncodeScaled instead.
inline uint8_t ProfileForChannels(int channels) {
    return channels >= 2 ? kProfileIQ : kProfileAudio;
}

// ---------------------------------------------------------------------------
// The scale of the reduced-depth IQ profile
// ---------------------------------------------------------------------------
//
// Order is the whole of it. The sender requantises BEFORE its predictor sees
// the samples and the receiver restores AFTER its predictor has produced them,
// so both ends run their filters on the values that are on the wire and stay
// bit-exact on the quantised grid. A decoder that restored before running its
// filters would diverge on its first packet.

// Quantise applies a shift to samples in place, rounding to nearest and
// clamping to the depth the shift implies. It is the sender's half of
// kProfileIQScaled; a decoder never performs it.
//
// Rounding rather than truncating matters: at ten bits half a step of the
// coarse grid is not a rounding detail, it is 42 dB below the peak. Clamping
// matters because rounding can carry a full-scale peak one step past the
// ceiling, and restoring that would wrap a full-scale positive sample to
// full-scale negative.
inline void Quantise(int16_t* samples, size_t n, unsigned shift) {
    if (shift == 0 || shift > kMaxShift) return;
    const int32_t half = static_cast<int32_t>(1) << (shift - 1);
    const int32_t lo = -(static_cast<int32_t>(32768) >> shift);
    const int32_t hi = static_cast<int32_t>(32767) >> shift;
    for (size_t i = 0; i < n; i++) {
        const int32_t v = static_cast<int32_t>(samples[i]) + half;
        // Floor division by 2^shift, written out because C++17 leaves the right
        // shift of a negative value implementation-defined.
        int32_t q;
        if (v < 0) {
            q = static_cast<int32_t>(~((~static_cast<uint32_t>(v)) >> shift));
        } else {
            q = static_cast<int32_t>(static_cast<uint32_t>(v) >> shift);
        }
        if (q < lo) q = lo;
        if (q > hi) q = hi;
        samples[i] = static_cast<int16_t>(q);
    }
}

// Restore is the decoder's half: the samples back onto the int16 grid.
//
// The saturation is not decoration. A decoder that has just reconstructed a
// value at the top of the quantised range shifts it to exactly 32767 or -32768;
// one that shifted without saturating could produce a value outside int16 and
// wrap it to the opposite rail, turning a strong signal into a loud artefact.
// Both ends must agree on it, so it is specified.
inline void Restore(int16_t* samples, size_t n, unsigned shift) {
    if (shift == 0) return;
    for (size_t i = 0; i < n; i++) {
        // |sample| <= 32768 and shift <= 15, so this stays inside int32; the
        // left shift goes through unsigned because shifting a negative value
        // left is undefined.
        int32_t r = static_cast<int32_t>(static_cast<uint32_t>(
            static_cast<int32_t>(samples[i])) << shift);
        if (r > 32767) r = 32767;
        if (r < -32768) r = -32768;
        samples[i] = static_cast<int16_t>(r);
    }
}

// ---------------------------------------------------------------------------
// Packet assembly
// ---------------------------------------------------------------------------

// Builds packets for one stream.
class StreamEncoder {
public:
    explicit StreamEncoder(uint64_t resync_nanos = kDefaultResyncNanos)
        : header_(resync_nanos) {}

    // Assemble one lossless packet into out (which is cleared first). samples
    // are int16 values, interleaved I/Q when channels is 2.
    bool Encode(const int16_t* samples, size_t n, uint64_t timestamp_nanos,
                int sample_rate, int channels, float baseband_power, float noise,
                std::vector<uint8_t>* out, std::string* err) {
        return EncodeImpl(samples, n, timestamp_nanos, sample_rate, channels,
                          baseband_power, noise, false, 0, out, err);
    }

    // Assemble one reduced-depth IQ packet: kProfileIQScaled, with the samples
    // requantised by shift before the predictor sees them and the shift itself
    // as the first byte of the body.
    //
    // shift must be 0..kMaxShift and channels must be 2 -- there is no scaled
    // real profile. The caller's samples are not modified; the requantisation
    // is lossy and is done on a copy.
    //
    // Which shift to use is the caller's policy and nothing on the wire depends
    // on how it was chosen. A shift of 0 is legal and codes the samples
    // exactly, but a sender that wants a lossless stream should call Encode and
    // profile 0 rather than declaring a lossy profile and shifting by nothing,
    // so that an archival stream is honestly labelled.
    bool EncodeScaled(const int16_t* samples, size_t n, uint64_t timestamp_nanos,
                      int sample_rate, int channels, float baseband_power,
                      float noise, unsigned shift, std::vector<uint8_t>* out,
                      std::string* err) {
        if (channels != 2)
            return detail::Fail(err, "the scaled profile is complex, so it needs 2 channels");
        if (shift > kMaxShift) return detail::Fail(err, "shift out of range 0..15");
        scratch_.assign(samples, samples + n);
        Quantise(scratch_.data(), scratch_.size(), shift);
        return EncodeImpl(scratch_.data(), n, timestamp_nanos, sample_rate, channels,
                          baseband_power, noise, true, shift, out, err);
    }

private:
    bool EncodeImpl(const int16_t* samples, size_t n, uint64_t timestamp_nanos,
                    int sample_rate, int channels, float baseband_power, float noise,
                    bool scaled, unsigned shift, std::vector<uint8_t>* out,
                    std::string* err) {
        if (channels <= 0) channels = 1;
        if (n == 0) return detail::Fail(err, "empty packet");

        // The predictor form follows the channel count, so a change rebuilds
        // the codec. Discarding the adaptation is correct rather than merely
        // acceptable: the new signal is a different one, and taps trained on
        // the old would predict it worse than a cold start. Crossing between
        // the lossless and the scaled IQ profile is the same event: the profile
        // id on the wire changes, so both ends rebuild.
        const uint8_t want = scaled ? kProfileIQScaled : ProfileForChannels(channels);
        if (want != profile_) {
            if (!Codec::Create(want, &codec_, err)) return false;
            profile_ = want;
        }

        // A complex profile consumes whole I/Q frames; a truncated final frame
        // would desynchronise the predictor, so drop it.
        if (codec_.samples_per_step() == 2 && (n % 2) != 0) n--;
        if (n == 0) return detail::Fail(err, "no whole frames in packet");

        // Detecting silence rather than being told about it catches every
        // source of it, and the scan costs a fraction of what coding one does.
        bool silent = true;
        for (size_t i = 0; i < n; i++) {
            if (samples[i] != 0) { silent = false; break; }
        }

        body_.clear();
        bool escape = false;
        if (silent) {
            if (!codec_.AdvanceSilence(n, err)) return false;
        } else {
            if (!codec_.Encode(samples, n, &body_, &escape, err)) return false;
        }

        Header h;
        h.timestamp_nanos = timestamp_nanos;
        h.sample_rate = sample_rate;
        h.channels = channels;
        h.sample_count = static_cast<int>(n);
        h.baseband_power = baseband_power;
        h.noise = noise;
        h.profile = want;
        h.escape = escape;
        h.silent = silent;

        out->clear();
        header_.Append(*out, h);
        // The shift leads the body rather than riding in the header: the flags
        // byte is full, and a silent packet has no body at all, so putting it
        // here costs a squelched or dead channel nothing.
        if (scaled && !silent) out->push_back(static_cast<uint8_t>(shift));
        out->insert(out->end(), body_.begin(), body_.end());
        return true;
    }

    HeaderEncoder header_;
    Codec codec_;
    uint8_t profile_ = 0xff;
    std::vector<uint8_t> body_;
    // Holds the requantised copy EncodeScaled codes, so the caller's samples
    // are never modified.
    std::vector<int16_t> scratch_;
};

// Reads packets for one stream.
//
// Every packet must pass through here, in order, from the first. A packet that
// is skipped -- even one whose samples the caller intends to discard -- leaves
// this side's filters where the encoder's no longer are, and everything after
// it decodes to noise.
class StreamDecoder {
public:
    bool Decode(const uint8_t* pkt, size_t len, Header* h,
                std::vector<int16_t>* samples, std::string* err) {
        size_t off = 0;
        if (!header_.Decode(pkt, len, h, &off, err)) return false;

        // The packet declares its own profile; nothing here infers it from the
        // channel count.
        if (h->profile != profile_) {
            if (!Codec::Create(h->profile, &codec_, err)) return false;
            profile_ = h->profile;
        }

        if (h->silent) {
            if (len != off) return detail::Fail(err, "silent packet carries a body");
            if (!codec_.AdvanceSilence(static_cast<size_t>(h->sample_count), err)) return false;
            samples->assign(static_cast<size_t>(h->sample_count), 0);
            return true;
        }

        // A scaled profile carries the shift as the first byte of the body. It
        // is read only here, after the silent case has returned: a silent
        // packet on a scaled profile carries no shift, because it carries no
        // body at all.
        unsigned shift = 0;
        if (codec_.scaled()) {
            if (len <= off) return detail::Fail(err, "scaled packet carries no shift byte");
            shift = pkt[off];
            if (shift > kMaxShift) return detail::Fail(err, "shift out of range 0..15");
            off++;
        }

        if (!codec_.Decode(pkt + off, len - off, static_cast<size_t>(h->sample_count),
                           h->escape, samples, err))
            return false;
        // Undo the scale only on the way out. The predictor above ran on the
        // values that were on the wire, exactly as the encoder's did.
        Restore(samples->data(), samples->size(), shift);
        return true;
    }

private:
    HeaderDecoder header_;
    Codec codec_;
    uint8_t profile_ = 0xff;
};

// ---------------------------------------------------------------------------
// Stream files
// ---------------------------------------------------------------------------
//
// The packet format says nothing about where one packet ends and the next
// begins, because on a message transport the frame boundary already does.
// Storing packets in a file needs that boundary back:
//
//     [length uint32 little-endian][packet bytes]  repeated to end of file
//
// There is no file header and no index. The convention is the extension .uiq.
// This container is a convenience, not part of the format.

// Reads one packet. Returns false at end of file with *eof set, or on error.
inline bool ReadPacket(std::istream& in, std::vector<uint8_t>* pkt, bool* eof,
                       std::string* err) {
    *eof = false;
    uint8_t prefix[4];
    in.read(reinterpret_cast<char*>(prefix), 4);
    if (in.gcount() == 0) { *eof = true; return false; }
    if (in.gcount() != 4) return detail::Fail(err, "truncated length prefix");
    const uint32_t n = detail::ReadU32(prefix);
    if (n == 0 || n > (1u << 26)) return detail::Fail(err, "implausible packet length");
    pkt->resize(n);
    in.read(reinterpret_cast<char*>(pkt->data()), n);
    if (static_cast<uint32_t>(in.gcount()) != n) return detail::Fail(err, "truncated packet");
    return true;
}

inline void WritePacket(std::ostream& out, const std::vector<uint8_t>& pkt) {
    uint8_t prefix[4];
    const uint32_t n = static_cast<uint32_t>(pkt.size());
    for (int i = 0; i < 4; i++) prefix[i] = static_cast<uint8_t>(n >> (8 * i));
    out.write(reinterpret_cast<const char*>(prefix), 4);
    out.write(reinterpret_cast<const char*>(pkt.data()), static_cast<std::streamsize>(pkt.size()));
}

}  // namespace ubersdr_iq

#endif  // UBERSDR_IQ_HPP
