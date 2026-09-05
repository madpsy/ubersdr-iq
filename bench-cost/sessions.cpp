// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins
//
// The many-sessions arm of SPEC.md section 10.2: what N simultaneous streams
// actually cost, rather than N times what one costs.
//
//   ./sessions FILE.pcm RATE CHANNELS FRAMES PACKETS JOBS [PACE]
//
// Each job gets its own encoder, its own decoder and its own copy of the
// samples, because separate sessions on a receiver share no working set. The
// figure reported is the sum of the jobs' thread CPU times, which is what a
// receiver's core budget is spent out of; the wall time is reported too so a
// run where the machine ran out of cores is visible rather than hidden.
//
// PACE=1 releases one packet per session per packet interval, which is what a
// receiver actually does and is the only setting whose numbers belong in a
// deployment claim. PACE=0 runs every session flat out, which on any machine
// with fewer cores than sessions measures the machine's saturation behaviour
// rather than the codec's cost.

#include "ubersdr_iq.hpp"

#include <atomic>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <fstream>
#include <string>
#include <thread>
#include <vector>

namespace uiq = ubersdr_iq;

static double NowUs(clockid_t clk) {
    struct timespec ts;
    clock_gettime(clk, &ts);
    return ts.tv_sec * 1e6 + ts.tv_nsec / 1e3;
}

int main(int argc, char** argv) {
    if (argc < 7) {
        std::fprintf(stderr,
                     "usage: %s FILE.pcm RATE CHANNELS FRAMES PACKETS JOBS [PACE]\n",
                     argv[0]);
        return 2;
    }
    const std::string path = argv[1];
    const int rate = std::atoi(argv[2]);
    const int channels = std::atoi(argv[3]);
    const int frames = std::atoi(argv[4]);
    const int packets = std::atoi(argv[5]);
    const int jobs = std::atoi(argv[6]);
    const bool pace = argc > 7 && std::atoi(argv[7]) != 0;
    // One packet interval, in nanoseconds.
    const int64_t period_ns =
        static_cast<int64_t>(frames) * 1000000000LL / rate;

    std::ifstream f(path, std::ios::binary);
    if (!f) { std::fprintf(stderr, "cannot open %s\n", path.c_str()); return 1; }
    std::vector<char> raw((std::istreambuf_iterator<char>(f)),
                          std::istreambuf_iterator<char>());
    const size_t per = static_cast<size_t>(frames) * channels;
    const size_t need = per * static_cast<size_t>(packets);
    if (raw.size() / 2 < need) {
        std::fprintf(stderr, "file holds %zu samples, run needs %zu\n", raw.size() / 2, need);
        return 1;
    }
    std::vector<int16_t> src(need);
    for (size_t i = 0; i < need; i++) {
        src[i] = static_cast<int16_t>(
            static_cast<uint8_t>(raw[2 * i]) |
            (static_cast<uint16_t>(static_cast<uint8_t>(raw[2 * i + 1])) << 8));
    }
    raw.clear(); raw.shrink_to_fit();

    std::vector<std::vector<int16_t>> mine(jobs, src);  // one copy per session
    src.clear(); src.shrink_to_fit();

    std::vector<double> enc_cpu(jobs, 0), dec_cpu(jobs, 0);
    std::atomic<int> ready{0};
    std::atomic<bool> go{false};

    auto work = [&](int id) {
        ready.fetch_add(1);
        while (!go.load()) { /* spin so all jobs start together */ }
        const int16_t* s = mine[id].data();
        std::string err;
        std::vector<std::vector<uint8_t>> stream;
        stream.reserve(packets);
        std::vector<uint8_t> pkt;
        uiq::StreamEncoder enc;
        struct timespec t0w;
        clock_gettime(CLOCK_MONOTONIC, &t0w);
        const int64_t base_ns = t0w.tv_sec * 1000000000LL + t0w.tv_nsec;
        const double c0 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        for (int p = 0; p < packets; p++) {
            if (pace) {
                struct timespec d;
                const int64_t at = base_ns + static_cast<int64_t>(p) * period_ns;
                d.tv_sec = at / 1000000000LL;
                d.tv_nsec = at % 1000000000LL;
                clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &d, nullptr);
            }
            const uint64_t ts = 1700000000000000000ull +
                                static_cast<uint64_t>(p) * static_cast<uint64_t>(frames) *
                                    1000000000ull / static_cast<uint64_t>(rate);
            if (!enc.Encode(s + static_cast<size_t>(p) * per, per, ts, rate, channels,
                            -999.0f, -999.0f, &pkt, &err)) {
                std::fprintf(stderr, "encode: %s\n", err.c_str());
                std::exit(1);
            }
            stream.push_back(pkt);
        }
        const double c1 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        enc_cpu[id] = (c1 - c0) / packets;

        uiq::StreamDecoder dec;
        uiq::Header h;
        std::vector<int16_t> out;
        clock_gettime(CLOCK_MONOTONIC, &t0w);
        const int64_t dbase_ns = t0w.tv_sec * 1000000000LL + t0w.tv_nsec;
        const double d0 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        for (int p = 0; p < packets; p++) {
            if (pace) {
                struct timespec d;
                const int64_t at = dbase_ns + static_cast<int64_t>(p) * period_ns;
                d.tv_sec = at / 1000000000LL;
                d.tv_nsec = at % 1000000000LL;
                clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &d, nullptr);
            }
            if (!dec.Decode(stream[p].data(), stream[p].size(), &h, &out, &err)) {
                std::fprintf(stderr, "decode: %s\n", err.c_str());
                std::exit(1);
            }
        }
        const double d1 = NowUs(CLOCK_THREAD_CPUTIME_ID);
        dec_cpu[id] = (d1 - d0) / packets;
    };

    std::vector<std::thread> th;
    for (int i = 0; i < jobs; i++) th.emplace_back(work, i);
    while (ready.load() < jobs) { }
    const double w0 = NowUs(CLOCK_MONOTONIC);
    go.store(true);
    for (auto& t : th) t.join();
    const double w1 = NowUs(CLOCK_MONOTONIC);

    double esum = 0, dsum = 0, emax = 0, dmax = 0;
    for (int i = 0; i < jobs; i++) {
        esum += enc_cpu[i]; dsum += dec_cpu[i];
        if (enc_cpu[i] > emax) emax = enc_cpu[i];
        if (dec_cpu[i] > dmax) dmax = dec_cpu[i];
    }
    const double pps = static_cast<double>(rate) / frames;
    std::printf("{\"impl\":\"cpp\",\"mode\":\"sessions\",\"rate\":%d,\"channels\":%d,"
                "\"frames\":%d,\"packets\":%d,\"jobs\":%d,\"paced\":%d,\"pps\":%.2f,"
                "\"enc_us_mean\":%.3f,\"enc_us_worst\":%.3f,"
                "\"dec_us_mean\":%.3f,\"dec_us_worst\":%.3f,"
                "\"enc_cores_total\":%.4f,\"dec_cores_total\":%.4f,"
                "\"wall_s\":%.3f}\n",
                rate, channels, frames, packets, jobs, pace ? 1 : 0, pps,
                esum / jobs, emax, dsum / jobs, dmax,
                esum * pps / 1e6, dsum * pps / 1e6, (w1 - w0) / 1e6);
    return 0;
}
