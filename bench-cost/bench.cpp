// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins
//
// Per-packet cost benchmark for SPEC.md section 10.2.
//
// Encodes and decodes one stream repeatedly with a single codec instance per
// repeat, so the predictor adaptation carries packet to packet exactly as it
// does in a real sender. Reports microseconds per packet, wall clock and
// thread CPU time, as a median over repeats with the spread.
//
//   ./bench FILE.pcm RATE CHANNELS FRAMES_PER_PACKET SHIFT PACKETS REPEATS
//
// SHIFT < 0 selects the lossless profile (0 for two channels, 1 for one).
// SHIFT >= 0 selects profile 2 with that shift, which needs two channels.
//
// Build with -DUIQ_BENCH_NO_LEAK to time a tree with the tap leakage of
// section 6.2.3 removed. That build is not conformant and is only for
// isolating what the leak costs.

#include "ubersdr_iq.hpp"

#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <fstream>
#include <string>
#include <vector>

namespace uiq = ubersdr_iq;

static double NowUs(clockid_t clk) {
    struct timespec ts;
    clock_gettime(clk, &ts);
    return ts.tv_sec * 1e6 + ts.tv_nsec / 1e3;
}

struct Stat {
    double median, lo, hi;
};

static Stat Summarise(std::vector<double> v) {
    std::sort(v.begin(), v.end());
    Stat s;
    const size_t n = v.size();
    s.median = (n % 2) ? v[n / 2] : 0.5 * (v[n / 2 - 1] + v[n / 2]);
    s.lo = v.front();
    s.hi = v.back();
    return s;
}

int main(int argc, char** argv) {
    if (argc < 8) {
        std::fprintf(stderr,
                     "usage: %s FILE.pcm RATE CHANNELS FRAMES SHIFT PACKETS REPEATS\n",
                     argv[0]);
        return 2;
    }
    const std::string path = argv[1];
    const int rate = std::atoi(argv[2]);
    const int channels = std::atoi(argv[3]);
    const int frames = std::atoi(argv[4]);
    const int shift = std::atoi(argv[5]);
    const int packets = std::atoi(argv[6]);
    const int repeats = std::atoi(argv[7]);

    std::ifstream f(path, std::ios::binary);
    if (!f) {
        std::fprintf(stderr, "cannot open %s\n", path.c_str());
        return 1;
    }
    std::vector<char> raw((std::istreambuf_iterator<char>(f)),
                          std::istreambuf_iterator<char>());
    std::vector<int16_t> src(raw.size() / 2);
    for (size_t i = 0; i < src.size(); i++) {
        src[i] = static_cast<int16_t>(
            static_cast<uint8_t>(raw[2 * i]) |
            (static_cast<uint16_t>(static_cast<uint8_t>(raw[2 * i + 1])) << 8));
    }
    raw.clear();
    raw.shrink_to_fit();

    const size_t per = static_cast<size_t>(frames) * channels;
    const size_t need = per * static_cast<size_t>(packets);
    if (src.size() < per) {
        std::fprintf(stderr, "file shorter than one packet\n");
        return 1;
    }
    // Tile the capture if it is shorter than the run. Every tile boundary is a
    // discontinuity the predictor has to re-adapt across; the number of them is
    // reported so it can be judged.
    int tiles = 1;
    std::vector<int16_t> samples;
    if (src.size() >= need) {
        samples.assign(src.begin(), src.begin() + need);
    } else {
        // Keep tiles frame-aligned so a complex stream never shifts I against Q.
        size_t whole = (src.size() / per) * per;
        if (whole == 0) whole = per;
        samples.reserve(need);
        while (samples.size() < need) {
            const size_t take = std::min(whole, need - samples.size());
            samples.insert(samples.end(), src.begin(), src.begin() + take);
        }
        tiles = static_cast<int>((need + whole - 1) / whole);
    }
    src.clear();
    src.shrink_to_fit();

    const bool scaled = shift >= 0;
    std::string err;

    // --- encode ------------------------------------------------------------
    std::vector<double> enc_wall, enc_cpu;
    std::vector<std::vector<uint8_t>> stream;
    size_t wire = 0;
    for (int r = 0; r < repeats + 2; r++) {
        uiq::StreamEncoder enc;
        std::vector<uint8_t> pkt;
        pkt.reserve(per * 2 + 64);
        const bool keep = (r == 0);
        if (keep) stream.reserve(packets);
        const double w0 = NowUs(CLOCK_MONOTONIC);
        const double c0 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        for (int p = 0; p < packets; p++) {
            const uint64_t ts = 1700000000000000000ull +
                                static_cast<uint64_t>(p) * static_cast<uint64_t>(frames) *
                                    1000000000ull / static_cast<uint64_t>(rate);
            const int16_t* s = &samples[static_cast<size_t>(p) * per];
            bool ok;
            if (scaled) {
                ok = enc.EncodeScaled(s, per, ts, rate, channels, -999.0f, -999.0f,
                                      static_cast<unsigned>(shift), &pkt, &err);
            } else {
                ok = enc.Encode(s, per, ts, rate, channels, -999.0f, -999.0f, &pkt, &err);
            }
            if (!ok) {
                std::fprintf(stderr, "encode: %s\n", err.c_str());
                return 1;
            }
            if (keep) {
                wire += pkt.size();
                stream.push_back(pkt);
            }
        }
        const double c1 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        const double w1 = NowUs(CLOCK_MONOTONIC);
        if (r >= 2) {  // first two repeats are warmup
            enc_wall.push_back((w1 - w0) / packets);
            enc_cpu.push_back((c1 - c0) / packets);
        }
    }

    // --- decode ------------------------------------------------------------
    std::vector<double> dec_wall, dec_cpu;
    for (int r = 0; r < repeats + 2; r++) {
        uiq::StreamDecoder dec;
        uiq::Header h;
        std::vector<int16_t> out;
        out.reserve(per);
        const double w0 = NowUs(CLOCK_MONOTONIC);
        const double c0 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        for (int p = 0; p < packets; p++) {
            if (!dec.Decode(stream[p].data(), stream[p].size(), &h, &out, &err)) {
                std::fprintf(stderr, "decode packet %d: %s\n", p, err.c_str());
                return 1;
            }
        }
        const double c1 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        const double w1 = NowUs(CLOCK_MONOTONIC);
        if (r >= 2) {
            dec_wall.push_back((w1 - w0) / packets);
            dec_cpu.push_back((c1 - c0) / packets);
        }
    }

    const Stat ew = Summarise(enc_wall), ec = Summarise(enc_cpu);
    const Stat dw = Summarise(dec_wall), dc = Summarise(dec_cpu);
    const double pps = static_cast<double>(rate) / frames;
    const double bits = 8.0 * wire / (static_cast<double>(packets) * per);

    std::printf(
        "{\"impl\":\"cpp\",\"file\":\"%s\",\"rate\":%d,\"channels\":%d,\"frames\":%d,"
        "\"shift\":%d,\"packets\":%d,\"repeats\":%d,\"tiles\":%d,\"pps\":%.2f,"
        "\"bits_per_sample\":%.3f,"
        "\"enc_us\":%.3f,\"enc_us_lo\":%.3f,\"enc_us_hi\":%.3f,"
        "\"dec_us\":%.3f,\"dec_us_lo\":%.3f,\"dec_us_hi\":%.3f,"
        "\"enc_cpu_us\":%.3f,\"dec_cpu_us\":%.3f,"
        "\"enc_core_pct\":%.4f,\"dec_core_pct\":%.4f,"
        "\"enc_msps\":%.3f,\"dec_msps\":%.3f}\n",
        path.c_str(), rate, channels, frames, shift, packets, repeats, tiles, pps, bits,
        ew.median, ew.lo, ew.hi, dw.median, dw.lo, dw.hi, ec.median, dc.median,
        ew.median * pps / 1e4, dw.median * pps / 1e4, per / ew.median, per / dw.median);
    return 0;
}
