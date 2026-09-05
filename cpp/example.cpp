// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// A worked example: compress a raw sample file, decompress it, check that the
// result is identical, and report what it cost.
//
//     make -C cpp example
//     ./cpp/example testdata/iq-384k.pcm 384000 2 360
//
// Sample files are raw little-endian int16, interleaved I/Q for two channels --
// the same layout as the body of a WAV file.

#include "ubersdr_iq.hpp"

#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <string>
#include <vector>

namespace uiq = ubersdr_iq;

int main(int argc, char** argv) {
    if (argc < 3) {
        std::printf("usage: %s FILE.pcm RATE [CHANNELS] [FRAMES_PER_PACKET]\n", argv[0]);
        return 2;
    }
    const std::string path = argv[1];
    const int rate = std::atoi(argv[2]);
    const int channels = argc > 3 ? std::atoi(argv[3]) : 1;
    const int frames = argc > 4 ? std::atoi(argv[4]) : 240;

    std::ifstream f(path, std::ios::binary);
    if (!f) {
        std::printf("cannot open %s\n", path.c_str());
        return 1;
    }
    std::vector<char> raw((std::istreambuf_iterator<char>(f)),
                          std::istreambuf_iterator<char>());
    std::vector<int16_t> samples(raw.size() / 2);
    for (size_t i = 0; i < samples.size(); i++) {
        samples[i] = static_cast<int16_t>(
            static_cast<uint8_t>(raw[2 * i]) |
            (static_cast<uint16_t>(static_cast<uint8_t>(raw[2 * i + 1])) << 8));
    }

    const size_t per = static_cast<size_t>(frames) * channels;
    if (samples.size() < per) {
        std::printf("file is shorter than one packet\n");
        return 1;
    }

    // One encoder for the whole stream. It carries the predictor's adaptation
    // from packet to packet, which is where most of the compression comes from,
    // so it must not be recreated per packet.
    uiq::StreamEncoder enc;
    std::vector<std::vector<uint8_t>> stream;
    std::string err;
    size_t wire = 0, coded_samples = 0;

    const auto t0 = std::chrono::steady_clock::now();
    for (size_t i = 0; i + per <= samples.size(); i += per) {
        // A real sender takes this from its receiver's clock. Here it is
        // derived from the sample index, which is what a steady stream looks
        // like.
        const uint64_t ts = 1700000000000000000ull +
                            (i / channels) * 1000000000ull / static_cast<uint64_t>(rate);
        std::vector<uint8_t> pkt;
        if (!enc.Encode(&samples[i], per, ts, rate, channels, -999.0f, -999.0f, &pkt,
                        &err)) {
            std::printf("encode: %s\n", err.c_str());
            return 1;
        }
        wire += pkt.size();
        coded_samples += per;
        stream.push_back(std::move(pkt));
    }
    const auto t1 = std::chrono::steady_clock::now();

    // One decoder for the whole stream, fed every packet in order. Skipping one
    // would leave its filters where the encoder's no longer are.
    uiq::StreamDecoder dec;
    std::vector<int16_t> back;
    back.reserve(coded_samples);
    for (size_t n = 0; n < stream.size(); n++) {
        uiq::Header h;
        std::vector<int16_t> s;
        if (!dec.Decode(stream[n].data(), stream[n].size(), &h, &s, &err)) {
            std::printf("decode packet %zu: %s\n", n, err.c_str());
            return 1;
        }
        back.insert(back.end(), s.begin(), s.end());
    }
    const auto t2 = std::chrono::steady_clock::now();

    for (size_t i = 0; i < coded_samples; i++) {
        if (back[i] != samples[i]) {
            std::printf("MISMATCH at sample %zu: %d != %d\n", i, back[i], samples[i]);
            return 1;
        }
    }

    const double enc_us =
        std::chrono::duration<double, std::micro>(t1 - t0).count() / stream.size();
    const double dec_us =
        std::chrono::duration<double, std::micro>(t2 - t1).count() / stream.size();
    const double pps = static_cast<double>(rate) / frames;

    std::printf("%s: %zu samples, %d Hz, %d channel(s), %d frames/packet\n",
                path.c_str(), coded_samples, rate, channels, frames);
    std::printf("  %zu packets, %zu -> %zu bytes, %.3fx (%.1f%% saved), lossless\n",
                stream.size(), coded_samples * 2, wire,
                static_cast<double>(coded_samples * 2) / wire,
                100.0 * (1.0 - static_cast<double>(wire) / (coded_samples * 2)));
    // The figure that matters for a receiver is not throughput but how much of
    // a core one stream costs, since it serves many at once.
    std::printf("  encode %6.1f us/packet  %5.2f%% of a core at %.0f packets/s\n",
                enc_us, enc_us * pps / 1e4, pps);
    std::printf("  decode %6.1f us/packet  %5.2f%% of a core\n", dec_us, dec_us * pps / 1e4);
    return 0;
}
