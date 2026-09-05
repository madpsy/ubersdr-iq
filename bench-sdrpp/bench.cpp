// Benchmark: SDR++ sdrpp_server streaming modes vs UberSDR-IQ, on the same
// lossless IQ capture.
//
// The SDR++ side is the real code path: process()/decompress-process() are
// transcribed verbatim from
//   core/src/dsp/compression/sample_stream_compressor.h
//   core/src/dsp/compression/sample_stream_decompressor.h
// and call the same volk kernels the real build links, with real libzstd at
// level 1 as core/src/server.cpp uses it.
//
// The UberSDR-IQ side is cpp/ubersdr_iq.hpp from the ubersdr-iq repository.

#include "ubersdr_iq.hpp"

#include <volk/volk.h>
#include <zstd.h>

#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <unistd.h>
#include <ctime>
#include <atomic>
#include <deque>
#include <thread>
#include <fstream>
#include <sstream>
#include <string>
#include <vector>

namespace uiq = ubersdr_iq;

struct complex_t { float re, im; };

enum PCMType { PCM_TYPE_I8 = 0, PCM_TYPE_I16 = 1, PCM_TYPE_F32 = 2 };

// --- verbatim from SDR++ sample_stream_compressor.h -------------------------
static bool g_fix_peak = false;
static int g_repeat = 1;
static double g_sat_window = 0.0;

static inline int sdrpp_compress(int count, PCMType pcmType, const complex_t* in, uint8_t* out) {
    uint16_t* compressionType = (uint16_t*)out;
    uint16_t* sampleType = (uint16_t*)&out[2];
    float* scaler = (float*)&out[4];
    void* dataBuf = &out[8];

    *compressionType = 0;
    *sampleType = pcmType;

    if (pcmType == PCM_TYPE_F32) {
        *scaler = 0;
        memcpy(dataBuf, in, count * sizeof(complex_t));
        return 8 + (count * sizeof(complex_t));
    }

    uint32_t maxIdx;
    volk_32f_index_max_32u(&maxIdx, (float*)in, count * 2);
    float maxVal = ((float*)in)[maxIdx];
    if (g_fix_peak) {
        // Scaler variant: largest magnitude rather than largest signed value.
        maxVal = 0.0f;
        for (int i = 0; i < count * 2; i++) {
            float a = std::fabs(((const float*)in)[i]);
            if (a > maxVal) maxVal = a;
        }
        if (maxVal == 0.0f) maxVal = 1.0f;
    }
    *scaler = maxVal;

    if (pcmType == PCM_TYPE_I8) {
        volk_32f_s32f_convert_8i((int8_t*)dataBuf, (float*)in, 128.0f / maxVal, count * 2);
        return 8 + (count * sizeof(int8_t) * 2);
    }
    else if (pcmType == PCM_TYPE_I16) {
        volk_32f_s32f_convert_16i((int16_t*)dataBuf, (float*)in, 32768.0f / maxVal, count * 2);
        return 8 + (count * sizeof(int16_t) * 2);
    }
    return count;
}

// --- verbatim from SDR++ sample_stream_decompressor.h -----------------------
static inline int sdrpp_decompress(int count, const uint8_t* in, complex_t* out) {
    uint16_t sampleType = *(uint16_t*)&in[2];
    float scaler = *(float*)&in[4];
    const void* dataBuf = &in[8];

    if (sampleType == PCM_TYPE_F32) {
        memcpy(out, dataBuf, count - 8);
        return (count - 8) / sizeof(complex_t);
    }
    else if (sampleType == PCM_TYPE_I16) {
        int outCount = (count - 8) / (sizeof(int16_t) * 2);
        volk_16i_s32f_convert_32f((float*)out, (int16_t*)dataBuf, 32768.0f / scaler, outCount * 2);
        return outCount;
    }
    else if (sampleType == PCM_TYPE_I8) {
        int outCount = (count - 8) / (sizeof(int8_t) * 2);
        volk_8i_s32f_convert_32f((float*)out, (int8_t*)dataBuf, 128.0f / scaler, outCount * 2);
        return outCount;
    }
    return 0;
}
// ---------------------------------------------------------------------------

// server.cpp wraps every baseband packet in an 8-byte PacketHeader.
static const size_t kSdrppPacketHeader = 8;

struct Result {
    const char* name;
    double wire_bytes;
    double enc_us;      // total
    double dec_us;      // total
    double snr_db;      // INFINITY if bit exact
    int64_t max_err;    // in int16 LSB
    int64_t clipped;    // samples that hit a converter rail
    int64_t mismatched; // int16 samples not recovered exactly
    bool lossless;
};

static std::vector<Result> results;
static std::string g_dump_prefix;

static void dump_pcm(const std::string& name, const std::vector<int16_t>& v) {
    if (g_dump_prefix.empty()) return;
    std::ofstream o(g_dump_prefix + name + ".pcm", std::ios::binary);
    o.write((const char*)v.data(), (std::streamsize)v.size() * 2);
}

static std::string cpu_model() {
    std::ifstream f("/proc/cpuinfo");
    std::string line;
    while (std::getline(f, line)) {
        if (line.rfind("model name", 0) == 0) {
            size_t c = line.find(':');
            if (c != std::string::npos) {
                size_t b = line.find_first_not_of(" \t", c + 1);
                return line.substr(b);
            }
        }
    }
    return "unknown";
}

// Mean of every core's current clock, in MHz. Sampled while the benchmark runs
// so the reported figures can be read against the clock that produced them.
static double cpu_mhz() {
    std::ifstream f("/proc/cpuinfo");
    std::string line;
    double sum = 0; int n = 0;
    while (std::getline(f, line)) {
        if (line.rfind("cpu MHz", 0) == 0) {
            size_t c = line.find(':');
            if (c != std::string::npos) { sum += std::atof(line.c_str() + c + 1); n++; }
        }
    }
    return n ? sum / n : 0.0;
}

// CPU time consumed by this thread, in microseconds. Not wall clock: the
// figures below are a claim about CPU consumption, and thread CPU time is what
// a scheduler actually charges the process.
static double now_us() {
    struct timespec ts;
    clock_gettime(CLOCK_THREAD_CPUTIME_ID, &ts);
    return (double)ts.tv_sec * 1e6 + (double)ts.tv_nsec / 1e3;
}

static int16_t f2i16(float v) {
    float r = v * 32768.0f;
    if (r > 32767.0f) r = 32767.0f;
    if (r < -32768.0f) r = -32768.0f;
    return (int16_t)std::lrintf(r);
}

static void run_sdrpp(const char* name, const char* tag, PCMType type, bool zstd, bool fix_peak,
                      const std::vector<complex_t>& in, const std::vector<int16_t>& orig,
                      size_t frames_per_block) {
    const size_t nblk = in.size() / frames_per_block;
    std::vector<uint8_t> buf(8 + frames_per_block * sizeof(complex_t) + 4096);
    std::vector<uint8_t> zbuf(ZSTD_compressBound(buf.size()));
    std::vector<complex_t> rec(frames_per_block);

    g_fix_peak = fix_peak;
    ZSTD_CCtx* cctx = ZSTD_createCCtx();
    ZSTD_DCtx* dctx = ZSTD_createDCtx();

    double wire = 0, enc = 0, dec = 0;
    double se = 0, sig = 0;
    int64_t maxerr = 0, clipped = 0, bad = 0;
    std::vector<int16_t> recon;
    if (!g_dump_prefix.empty()) recon.reserve(nblk * frames_per_block * 2);

    for (int rep = 0; rep < g_repeat; rep++) {
    const bool score = (rep == 0);
    for (size_t b = 0; b < nblk; b++) {
        const complex_t* src = &in[b * frames_per_block];

        double t0 = now_us();
        int n = sdrpp_compress((int)frames_per_block, type, src, buf.data());
        size_t sent = (size_t)n;
        if (zstd) {
            sent = ZSTD_compressCCtx(cctx, zbuf.data(), zbuf.size(), buf.data(), n, 1);
        }
        enc += now_us() - t0;
        wire += kSdrppPacketHeader + sent;

        t0 = now_us();
        const uint8_t* dsrc = buf.data();
        int dn = n;
        if (zstd) {
            dn = (int)ZSTD_decompressDCtx(dctx, buf.data(), buf.size(), zbuf.data(), sent);
            dsrc = buf.data();
        }
        sdrpp_decompress(dn, dsrc, rec.data());
        dec += now_us() - t0;

        if (!score) continue;
        // Error against the original int16 samples.
        for (size_t i = 0; i < frames_per_block; i++) {
            const float o[2] = { src[i].re, src[i].im };
            const float r[2] = { rec[i].re, rec[i].im };
            for (int c = 0; c < 2; c++) {
                double d = (double)r[c] - (double)o[c];
                se += d * d;
                sig += (double)o[c] * (double)o[c];
                int16_t oi = orig[(b * frames_per_block + i) * 2 + c];
                int16_t ri = f2i16(r[c]);
                if (!g_dump_prefix.empty()) recon.push_back(ri);
                int64_t e = std::llabs((int64_t)ri - (int64_t)oi);
                if (e > maxerr) maxerr = e;
                if (e) bad++;
            }
        }
        // Count samples the converter railed (input magnitude above the scaler).
        if (type != PCM_TYPE_F32 && !fix_peak) {
            uint32_t mi; volk_32f_index_max_32u(&mi, (float*)src, (unsigned)frames_per_block * 2);
            float mv = ((float*)src)[mi];
            for (size_t i = 0; i < frames_per_block * 2; i++) {
                float v = ((const float*)src)[i];
                if (v < -mv) clipped++;
            }
        }
    }
    }
    enc /= g_repeat; dec /= g_repeat; wire /= g_repeat;
    ZSTD_freeCCtx(cctx);
    ZSTD_freeDCtx(dctx);
    g_fix_peak = false;
    dump_pcm(tag, recon);

    Result r;
    r.name = name;
    r.wire_bytes = wire;
    r.enc_us = enc;
    r.dec_us = dec;
    r.snr_db = (se == 0.0) ? INFINITY : 10.0 * std::log10(sig / se);
    r.max_err = maxerr;
    r.clipped = clipped;
    r.mismatched = bad;
    r.lossless = (bad == 0);
    results.push_back(r);
}

static void run_uiq(const std::vector<int16_t>& orig, int rate, int channels,
                    size_t frames_per_block) {
    const size_t per = frames_per_block * (size_t)channels;
    const size_t nblk = orig.size() / per;

    std::string err;
    std::vector<std::vector<uint8_t>> stream;
    stream.reserve(nblk);

    double wire = 0, te = 0, td = 0;
    for (int rep = 0; rep < g_repeat; rep++) {
    // A fresh encoder per pass: each repetition is an identical, independent
    // measurement rather than a continuation of the last one.
    uiq::StreamEncoder enc;
    stream.clear();
    wire = 0;
    for (size_t b = 0; b < nblk; b++) {
        const uint64_t ts = 1700000000000000000ull +
                            (uint64_t)(b * frames_per_block) * 1000000000ull / (uint64_t)rate;
        std::vector<uint8_t> pkt;
        double t0 = now_us();
        if (!enc.Encode(&orig[b * per], per, ts, rate, channels, -999.0f, -999.0f, &pkt, &err)) {
            std::fprintf(stderr, "uiq encode: %s\n", err.c_str());
            std::exit(1);
        }
        te += now_us() - t0;
        wire += pkt.size();
        stream.push_back(std::move(pkt));
    }
    }

    int64_t bad = 0;
    std::vector<int16_t> out;
    uiq::Header h;
    for (int rep = 0; rep < g_repeat; rep++) {
    uiq::StreamDecoder dec;
    for (size_t b = 0; b < nblk; b++) {
        double t0 = now_us();
        if (!dec.Decode(stream[b].data(), stream[b].size(), &h, &out, &err)) {
            std::fprintf(stderr, "uiq decode: %s\n", err.c_str());
            std::exit(1);
        }
        td += now_us() - t0;
        if (rep) continue;
        for (size_t i = 0; i < out.size(); i++) {
            if (out[i] != orig[b * per + i]) bad++;
        }
    }
    }
    te /= g_repeat; td /= g_repeat;

    Result r;
    r.name = "UberSDR-IQ (re-encoded here)";
    r.wire_bytes = wire;
    r.enc_us = te;
    r.dec_us = td;
    r.snr_db = INFINITY;
    r.max_err = 0;
    r.clipped = 0;
    r.mismatched = bad;
    r.lossless = (bad == 0);
    results.push_back(r);
}

// --- reduced-depth arm: profile 2, one shift per packet ---------------------
// The shift schedule is not chosen here. It comes from `shifts.py`, which is
// ../bitdepth-iq/margin_rule.py's per-packet policy: a client asks for a margin
// in decibels and the depth honouring it falls out of the packet's crest factor
// and the band's spectral peakiness. Only the shift crosses into this file,
// because only the shift is on the wire (SPEC.md §6.6.4). The quantisation
// itself is StreamEncoder::EncodeScaled's, which is §6.6.2's, not numpy's.

struct ShiftArm {
    std::string label;              // e.g. "26 dB"
    std::string tag;                // e.g. "26dB", for the dumped .pcm
    std::vector<uint8_t> shifts;    // one per packet
};
static std::deque<ShiftArm> g_arms;
// A deque, not a vector: Result::name is a const char* into one of these, and a
// vector's reallocation would leave every earlier one dangling.
static std::deque<std::string> g_names;

static void run_uiq_scaled(const ShiftArm& arm, const std::vector<int16_t>& orig,
                           int rate, int channels, size_t frames_per_block) {
    const size_t per = frames_per_block * (size_t)channels;
    const size_t nblk = orig.size() / per;
    if (arm.shifts.size() < nblk) {
        std::fprintf(stderr, "shift schedule for %s has %zu entries, need %zu\n",
                     arm.label.c_str(), arm.shifts.size(), nblk);
        std::exit(1);
    }

    std::string err;
    std::vector<std::vector<uint8_t>> stream;
    stream.reserve(nblk);

    double wire = 0, te = 0, td = 0;
    for (int rep = 0; rep < g_repeat; rep++) {
    uiq::StreamEncoder enc;
    stream.clear();
    wire = 0;
    for (size_t b = 0; b < nblk; b++) {
        const uint64_t ts = 1700000000000000000ull +
                            (uint64_t)(b * frames_per_block) * 1000000000ull / (uint64_t)rate;
        std::vector<uint8_t> pkt;
        double t0 = now_us();
        if (!enc.EncodeScaled(&orig[b * per], per, ts, rate, channels, -999.0f, -999.0f,
                              arm.shifts[b], &pkt, &err)) {
            std::fprintf(stderr, "uiq encode scaled: %s\n", err.c_str());
            std::exit(1);
        }
        te += now_us() - t0;
        wire += pkt.size();
        stream.push_back(std::move(pkt));
    }
    }

    // Error against the original int16 samples, scored the way the SDR++ lossy
    // rows are scored so the two are on one axis.
    double se = 0, sig = 0;
    int64_t maxerr = 0, bad = 0, clipped = 0;
    std::vector<int16_t> recon;
    if (!g_dump_prefix.empty()) recon.reserve(nblk * per);
    std::vector<int16_t> out;
    uiq::Header h;
    for (int rep = 0; rep < g_repeat; rep++) {
    uiq::StreamDecoder dec;
    for (size_t b = 0; b < nblk; b++) {
        double t0 = now_us();
        if (!dec.Decode(stream[b].data(), stream[b].size(), &h, &out, &err)) {
            std::fprintf(stderr, "uiq decode scaled: %s\n", err.c_str());
            std::exit(1);
        }
        td += now_us() - t0;
        if (rep) continue;
        for (size_t i = 0; i < out.size(); i++) {
            const int64_t o = orig[b * per + i], r = out[i];
            const double d = (double)(r - o);
            se += d * d; sig += (double)o * (double)o;
            const int64_t e = std::llabs(r - o);
            if (e > maxerr) maxerr = e;
            if (e) bad++;
            if (!g_dump_prefix.empty()) recon.push_back(out[i]);
        }
        // Samples the quantiser had to clamp: the profile-2 counterpart of an
        // SDR++ converter railing. §6.6.2 clamps to the depth the shift implies
        // because rounding can carry a peak one step past the ceiling.
        const unsigned s = arm.shifts[b];
        if (s > 0 && s <= uiq::kMaxShift) {
            const int32_t half = (int32_t)1 << (s - 1);
            const int32_t lo = -((int32_t)32768 >> s), hi = (int32_t)32767 >> s;
            for (size_t i = 0; i < per; i++) {
                const int32_t v = (int32_t)orig[b * per + i] + half;
                const int32_t q = (v < 0) ? (int32_t)(~((~(uint32_t)v) >> s))
                                          : (int32_t)((uint32_t)v >> s);
                if (q < lo || q > hi) clipped++;
            }
        }
    }
    }
    te /= g_repeat; td /= g_repeat;
    dump_pcm("uiq_" + arm.tag, recon);

    g_names.push_back("UberSDR-IQ, " + arm.label + " margin");
    Result r;
    r.name = g_names.back().c_str();
    r.wire_bytes = wire;
    r.enc_us = te;
    r.dec_us = td;
    r.snr_db = (se == 0.0) ? INFINITY : 10.0 * std::log10(sig / se);
    r.max_err = maxerr;
    r.clipped = clipped;
    r.mismatched = bad;
    r.lossless = (bad == 0);
    results.push_back(r);
}


// ---------------------------------------------------------------------------
// Saturation test: how many concurrent real-time streams this machine actually
// sustains. Each thread runs an independent encoder over the same samples, flat
// out, for a fixed wall-clock window. Aggregate throughput divided by one
// stream's sample rate is the number of streams that fit. Unlike scaling a
// single-thread figure by the core count, this pays for SMT contention, shared
// L3, memory bandwidth and whatever the clock does under all-core load.
//
// Each thread works on its own copy of the samples. Sharing one buffer would
// let every thread hit the same cache lines, which no set of real concurrent
// sessions does; the copies put the memory traffic back.

static double wall_us() {
    using namespace std::chrono;
    return duration<double, std::micro>(steady_clock::now().time_since_epoch()).count();
}

// Returns aggregate int16 samples encoded per second across all threads.
static double saturate_sdrpp(PCMType type, bool zstd, bool fix_peak,
                             const std::vector<complex_t>& in, size_t fpb,
                             int threads, double window_s) {
    const size_t nblk = in.size() / fpb;
    std::atomic<bool> go{false}, stop{false};
    std::atomic<int> ready{0};
    std::atomic<long long> total{0};
    std::vector<std::thread> pool;

    for (int t = 0; t < threads; t++) {
        pool.emplace_back([&]() {
            const std::vector<complex_t> mine(in);  // this stream's own samples
            std::vector<uint8_t> buf(8 + fpb * sizeof(complex_t) + 4096);
            std::vector<uint8_t> zbuf(ZSTD_compressBound(buf.size()));
            ZSTD_CCtx* cctx = ZSTD_createCCtx();
            long long done = 0;
            ready.fetch_add(1, std::memory_order_release);
            while (!go.load(std::memory_order_acquire)) { }
            while (!stop.load(std::memory_order_relaxed)) {
                for (size_t b = 0; b < nblk && !stop.load(std::memory_order_relaxed); b++) {
                    int n = sdrpp_compress((int)fpb, type, &mine[b * fpb], buf.data());
                    if (zstd) ZSTD_compressCCtx(cctx, zbuf.data(), zbuf.size(), buf.data(), n, 1);
                    done += (long long)fpb * 2;
                }
            }
            ZSTD_freeCCtx(cctx);
            total.fetch_add(done, std::memory_order_relaxed);
        });
    }
    g_fix_peak = fix_peak;
    while (ready.load(std::memory_order_acquire) < threads) { }
    const double t0 = wall_us();
    go.store(true, std::memory_order_release);
    while (wall_us() - t0 < window_s * 1e6) { }
    stop.store(true, std::memory_order_relaxed);
    for (auto& th : pool) th.join();
    const double elapsed = (wall_us() - t0) / 1e6;
    g_fix_peak = false;
    return (double)total.load() / elapsed;
}

// arm == nullptr is the lossless profile; otherwise profile 2 on that arm's
// shift schedule.
static double saturate_uiq(const std::vector<int16_t>& orig, int rate, int channels,
                           size_t fpb, int threads, double window_s,
                           const ShiftArm* arm = nullptr) {
    const size_t per = fpb * (size_t)channels;
    const size_t nblk = orig.size() / per;
    std::atomic<bool> go{false}, stop{false};
    std::atomic<int> ready{0};
    std::atomic<long long> total{0};
    std::vector<std::thread> pool;

    for (int t = 0; t < threads; t++) {
        pool.emplace_back([&]() {
            const std::vector<int16_t> mine(orig);  // this stream's own samples
            uiq::StreamEncoder enc;
            std::vector<uint8_t> pkt;
            std::string err;
            long long done = 0;
            uint64_t seq = 0;
            ready.fetch_add(1, std::memory_order_release);
            while (!go.load(std::memory_order_acquire)) { }
            while (!stop.load(std::memory_order_relaxed)) {
                for (size_t b = 0; b < nblk && !stop.load(std::memory_order_relaxed); b++) {
                    const uint64_t ts = 1700000000000000000ull +
                        (seq++) * (uint64_t)fpb * 1000000000ull / (uint64_t)rate;
                    const bool ok = arm
                        ? enc.EncodeScaled(&mine[b * per], per, ts, rate, channels,
                                           -999.0f, -999.0f, arm->shifts[b], &pkt, &err)
                        : enc.Encode(&mine[b * per], per, ts, rate, channels,
                                     -999.0f, -999.0f, &pkt, &err);
                    if (!ok) return;
                    done += (long long)per;
                }
            }
            total.fetch_add(done, std::memory_order_relaxed);
        });
    }
    while (ready.load(std::memory_order_acquire) < threads) { }
    const double t0 = wall_us();
    go.store(true, std::memory_order_release);
    while (wall_us() - t0 < window_s * 1e6) { }
    stop.store(true, std::memory_order_relaxed);
    for (auto& th : pool) th.join();
    return (double)total.load() / ((wall_us() - t0) / 1e6);
}

int main(int argc, char** argv) {
    if (argc < 4) {
        std::printf("usage: %s FILE.pcm RATE FRAMES_PER_BLOCK [WIRE_BYTES] [DUMP_PREFIX]"
                    " [REPEAT] [SAT_SECONDS] [MARGIN_DB:SHIFTS.bin ...]\n", argv[0]);
        return 2;
    }
    const std::string path = argv[1];
    const int rate = std::atoi(argv[2]);
    const size_t fpb = (size_t)std::atoll(argv[3]);
    // Coded bytes of the committed .uiq, for a cross-implementation check:
    // the Go encoder wrote that container, the C++ one re-encodes here.
    const double live_wire = argc > 4 ? std::atof(argv[4]) : 0.0;
    if (argc > 5) g_dump_prefix = argv[5];
    if (argc > 6) g_repeat = std::atoi(argv[6]);
    if (argc > 7) g_sat_window = std::atof(argv[7]);

    // Reduced-depth arms: one MARGIN_DB:PATH per arm, PATH holding one shift
    // byte per packet as `shifts.py` wrote it.
    for (int i = 8; i < argc; i++) {
        const std::string a = argv[i];
        const size_t c = a.find(':');
        if (c == std::string::npos) {
            std::printf("expected MARGIN_DB:SHIFTS.bin, got %s\n", a.c_str());
            return 2;
        }
        ShiftArm arm;
        arm.label = a.substr(0, c) + " dB";
        arm.tag = a.substr(0, c) + "dB";
        std::ifstream sf(a.substr(c + 1), std::ios::binary);
        if (!sf) { std::printf("cannot open %s\n", a.c_str() + c + 1); return 1; }
        arm.shifts.assign((std::istreambuf_iterator<char>(sf)),
                          std::istreambuf_iterator<char>());
        g_arms.push_back(std::move(arm));
    }

    std::ifstream f(path, std::ios::binary);
    if (!f) { std::printf("cannot open %s\n", path.c_str()); return 1; }
    std::vector<char> raw((std::istreambuf_iterator<char>(f)), std::istreambuf_iterator<char>());
    std::vector<int16_t> samples(raw.size() / 2);
    for (size_t i = 0; i < samples.size(); i++) {
        samples[i] = (int16_t)((uint8_t)raw[2*i] | ((uint16_t)(uint8_t)raw[2*i+1] << 8));
    }
    const size_t nframes = samples.size() / 2;

    // What a source module hands SDR++: float32 complex normalised to +/-1.
    std::vector<complex_t> cf(nframes);
    for (size_t i = 0; i < nframes; i++) {
        cf[i].re = (float)samples[2*i]   / 32768.0f;
        cf[i].im = (float)samples[2*i+1] / 32768.0f;
    }

    const size_t nblk = nframes / fpb;
    const size_t used_frames = nblk * fpb;
    const double secs = (double)used_frames / rate;

    std::printf("capture   %s\n", path.c_str());
    std::printf("          %zu frames, %d Hz, 2 ch, %.3f s, %zu blocks of %zu frames\n",
                used_frames, rate, secs, nblk, fpb);
    std::printf("raw int16 %.0f bytes (%.1f kB/s)\n\n",
                (double)used_frames * 4, (double)used_frames * 4 / secs / 1000.0);

    run_sdrpp("SDR++ float32", "float32",          PCM_TYPE_F32, false, false, cf, samples, fpb);
    run_sdrpp("SDR++ float32 + zstd-1", "float32_plus_zstd-1", PCM_TYPE_F32, true,  false, cf, samples, fpb);
    run_sdrpp("SDR++ int16 (default)", "int16_default",  PCM_TYPE_I16, false, false, cf, samples, fpb);
    run_sdrpp("SDR++ int16 + zstd-1", "int16_plus_zstd-1",   PCM_TYPE_I16, true,  false, cf, samples, fpb);
    run_sdrpp("SDR++ int8", "int8",             PCM_TYPE_I8,  false, false, cf, samples, fpb);
    run_sdrpp("SDR++ int8 + zstd-1", "int8_plus_zstd-1",    PCM_TYPE_I8,  true,  false, cf, samples, fpb);
    run_sdrpp("SDR++ int16, peak fixed", "int16_peak_fixed", PCM_TYPE_I16, false, true, cf, samples, fpb);
    run_sdrpp("SDR++ int8, peak fixed", "int8_peak_fixed",  PCM_TYPE_I8,  false, true, cf, samples, fpb);
    const std::vector<int16_t> used(samples.begin(), samples.begin() + used_frames * 2);
    run_uiq(used, rate, 2, fpb);
    for (const auto& arm : g_arms) run_uiq_scaled(arm, used, rate, 2, fpb);

    for (const auto& arm : g_arms) {
        double mean = 0; unsigned lo = 255, hi = 0;
        for (size_t b = 0; b < nblk; b++) {
            const unsigned s = arm.shifts[b];
            mean += s; if (s < lo) lo = s; if (s > hi) hi = s;
        }
        std::printf("profile 2 at a %s margin: shift %u..%u, mean %.3f\n",
                    arm.label.c_str(), lo, hi, mean / (double)nblk);
    }
    if (!g_arms.empty()) std::printf("\n");

    const double rawbytes = (double)used_frames * 4;

    std::printf("%-28s %10s %9s %8s %10s %9s %9s\n",
                "mode", "kB/s", "vs int16", "lossless", "SNR dB", "max err", "clipped");
    for (const auto& r : results) {
        char snr[32];
        if (std::isinf(r.snr_db)) std::snprintf(snr, sizeof snr, "%s", "exact");
        else std::snprintf(snr, sizeof snr, "%.1f", r.snr_db);
        std::printf("%-28s %10.1f %8.3fx %8s %10s %9lld %9lld\n",
                    r.name, r.wire_bytes / secs / 1000.0, rawbytes / r.wire_bytes,
                    r.lossless ? "yes" : "NO", snr,
                    (long long)r.max_err, (long long)r.clipped);
    }
    if (live_wire > 0) {
        std::printf("%-28s %10.1f %8.3fx %8s %10s %9d %9d\n",
                    "UberSDR-IQ (committed .uiq)", live_wire / secs / 1000.0,
                    rawbytes / live_wire, "yes", "exact", 0, 0);
    }

    const double nsamples = (double)used_frames * 2;
    std::printf("\n%-28s %8s %8s %8s %9s %8s %9s %8s\n", "mode",
                "enc us", "dec us", "Ms/s", "CPU ms/s", "% core", "CPU s/h", "streams");
    for (const auto& r : results) {
        const double blocks = (double)nblk;
        const double cpu_ms_per_s = (r.enc_us / 1000.0) / secs;  // ms of CPU per s of signal
        const double core = cpu_ms_per_s / 10.0;                 // % of one core
        std::printf("%-28s %8.1f %8.1f %8.1f %9.3f %7.3f%% %9.2f %8.0f\n", r.name,
                    r.enc_us / blocks, r.dec_us / blocks,
                    nsamples / r.enc_us, cpu_ms_per_s, core,
                    cpu_ms_per_s * 3.6, 100.0 / core);
    }
    std::printf("\nAll figures are encoder-side thread CPU time; a decoder pays about the\n"
                "same (see 'dec us'). Per one real-time %d Hz stream:\n"
                "  enc us / dec us  CPU microseconds per block of %zu frames\n"
                "  Ms/s             int16 samples encoded per second of CPU, millions\n"
                "  CPU ms/s         CPU milliseconds spent per second of signal\n"
                "  %% core           share of one core, = CPU ms/s / 10\n"
                "  CPU s/h          CPU seconds burned per hour of streaming\n"
                "  streams          concurrent real-time streams one core sustains\n",
                rate, fpb);
    std::printf("\nmachine: %s\n         %ld logical CPUs, %.0f MHz mean clock during this run\n",
                cpu_model().c_str(), sysconf(_SC_NPROCESSORS_ONLN), cpu_mhz());

    // --- saturation: concurrent real-time streams, measured in parallel ------
    if (g_sat_window > 0.0) {
        const int counts[] = {1, 2, 4, 8, 16};
        const int ncounts = 5;
        const double per_stream = (double)rate * 2.0;  // int16 samples per second

        struct SatMode { std::string name; PCMType type; bool zstd; bool fix; bool uiq;
                         const ShiftArm* arm; };
        std::vector<SatMode> modes = {
            {"SDR++ float32",              PCM_TYPE_F32, false, false, false, nullptr},
            {"SDR++ float32 + zstd-1",     PCM_TYPE_F32, true,  false, false, nullptr},
            {"SDR++ int16 (server default)", PCM_TYPE_I16, false, false, false, nullptr},
            {"SDR++ int16 + zstd-1",       PCM_TYPE_I16, true,  false, false, nullptr},
            {"SDR++ int8",                 PCM_TYPE_I8,  false, false, false, nullptr},
            {"SDR++ int8 + zstd-1",        PCM_TYPE_I8,  true,  false, false, nullptr},
            {"SDR++ int16, peak fixed",    PCM_TYPE_I16, false, true,  false, nullptr},
            {"SDR++ int8, peak fixed",     PCM_TYPE_I8,  false, true,  false, nullptr},
            {"UberSDR-IQ",                 PCM_TYPE_I16, false, false, true,  nullptr},
        };
        for (const auto& a : g_arms)
            modes.push_back({"UberSDR-IQ, " + a.label + " margin", PCM_TYPE_I16,
                             false, false, true, &a});

        std::printf("\n\nconcurrent real-time %d Hz streams sustained, encoders running in\n"
                    "parallel (%.1f s per point, %d logical CPUs):\n\n", rate,
                    g_sat_window, (int)sysconf(_SC_NPROCESSORS_ONLN));
        std::printf("%-30s", "mode");
        for (int i = 0; i < ncounts; i++) std::printf(" %9d", counts[i]);
        std::printf(" %10s\n", "peak");
        std::printf("%-30s", "");
        for (int i = 0; i < ncounts; i++) std::printf(" %9s", "thread(s)");
        std::printf(" %10s\n", "");

        for (const auto& m : modes) {
            std::printf("%-30s", m.name.c_str());
            double best = 0;
            for (int i = 0; i < ncounts; i++) {
                const double agg = m.uiq
                    ? saturate_uiq(samples, rate, 2, fpb, counts[i], g_sat_window, m.arm)
                    : saturate_sdrpp(m.type, m.zstd, m.fix, cf, fpb, counts[i], g_sat_window);
                const double streams = agg / per_stream;
                if (streams > best) best = streams;
                std::printf(" %9.0f", streams);
                std::fflush(stdout);
            }
            std::printf(" %10.0f\n", best);
        }
        std::printf("\nEach thread runs an independent encoder flat out; the figure is aggregate\n"
                    "throughput divided by one stream's %.0f kB/s of int16. Encoder side; a\n"
                    "decoder costs about the same. Mean clock now %.0f MHz.\n",
                    per_stream * 2 / 1000.0, cpu_mhz());
    }
    return 0;
}
