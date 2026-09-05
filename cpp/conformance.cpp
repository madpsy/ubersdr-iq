// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// Conformance tests for the C++ implementation.
//
//     make -C cpp && ./cpp/conformance testdata
//
// Two checks per vector, for different reasons.
//
// Decoding is the correctness requirement: the .uiq stream must reconstruct the
// .pcm samples exactly. Anything that reads this format has to pass it.
//
// Encoding is the sharper one: the .pcm samples must produce the .uiq stream
// byte for byte. The format does not require that -- an encoder may
// legitimately choose a different k or take the escape earlier -- but
// byte-identity catches a predictor that is subtly wrong in a way its own
// decoder would reproduce and therefore hide.

#include "ubersdr_iq.hpp"

#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <string>
#include <vector>

namespace uiq = ubersdr_iq;

namespace {

int failures = 0;

void Check(bool ok, const std::string& what) {
    if (!ok) {
        std::printf("  FAIL  %s\n", what.c_str());
        failures++;
    }
}

std::vector<int16_t> LoadPCM(const std::string& path) {
    std::ifstream f(path, std::ios::binary);
    if (!f) {
        std::printf("cannot open %s\n", path.c_str());
        std::exit(1);
    }
    std::vector<char> raw((std::istreambuf_iterator<char>(f)),
                          std::istreambuf_iterator<char>());
    std::vector<int16_t> out(raw.size() / 2);
    for (size_t i = 0; i < out.size(); i++) {
        out[i] = static_cast<int16_t>(static_cast<uint8_t>(raw[2 * i]) |
                                      (static_cast<uint16_t>(static_cast<uint8_t>(raw[2 * i + 1])) << 8));
    }
    return out;
}

std::vector<std::vector<uint8_t>> LoadPackets(const std::string& path) {
    std::ifstream f(path, std::ios::binary);
    if (!f) {
        std::printf("cannot open %s\n", path.c_str());
        std::exit(1);
    }
    std::vector<std::vector<uint8_t>> out;
    for (;;) {
        std::vector<uint8_t> pkt;
        bool eof = false;
        std::string err;
        if (!uiq::ReadPacket(f, &pkt, &eof, &err)) {
            if (eof) break;
            std::printf("%s: %s\n", path.c_str(), err.c_str());
            std::exit(1);
        }
        out.push_back(pkt);
    }
    return out;
}

struct Vector {
    const char* name;
    int rate;
    int channels;
    int frames;  // frames per packet
};

// The timestamp schedule the vectors were produced with.
constexpr uint64_t kStartNs = 1700000000000000000ull;

const Vector kVectors[] = {
    {"audio-cw-12k", 12000, 1, 240},
    {"audio-usb-12k", 12000, 1, 240},
    {"iq-12k", 12000, 2, 240},
    {"iq-384k", 384000, 2, 360},
    {"iq-384k-mw", 384000, 2, 360},
    // Generated rather than captured: a Rice codeword whose unary run empties a
    // full 64-bit accumulator, so consuming it is a shift by 64. That is
    // undefined in C++ and this is the only vector that contains one.
    {"rice-63", 96000, 2, 240},
};

void TestVectors(const std::string& dir) {
    for (const Vector& v : kVectors) {
        const std::vector<int16_t> want = LoadPCM(dir + "/" + v.name + ".pcm");
        const auto packets = LoadPackets(dir + "/" + v.name + ".uiq");

        // Decode.
        uiq::StreamDecoder dec;
        std::vector<int16_t> got;
        size_t wire = 0;
        bool ok = true;
        for (size_t n = 0; n < packets.size(); n++) {
            uiq::Header h;
            std::vector<int16_t> s;
            std::string err;
            if (!dec.Decode(packets[n].data(), packets[n].size(), &h, &s, &err)) {
                std::printf("  FAIL  %s packet %zu: %s\n", v.name, n, err.c_str());
                failures++;
                ok = false;
                break;
            }
            Check(h.sample_rate == v.rate && h.channels == v.channels,
                  std::string(v.name) + ": header metadata");
            got.insert(got.end(), s.begin(), s.end());
            wire += packets[n].size();
        }
        if (ok) {
            Check(got == want, std::string(v.name) + ": decode is not lossless");
        }

        // Encode, byte for byte.
        uiq::StreamEncoder enc;
        const size_t per = static_cast<size_t>(v.frames) * v.channels;
        size_t n = 0;
        for (size_t i = 0; i + per <= want.size(); i += per) {
            const uint64_t ts = kStartNs + (i / v.channels) * 1000000000ull / v.rate;
            std::vector<uint8_t> pkt;
            std::string err;
            if (!enc.Encode(&want[i], per, ts, v.rate, v.channels, -999.0f, -999.0f,
                            &pkt, &err)) {
                std::printf("  FAIL  %s encode packet %zu: %s\n", v.name, n, err.c_str());
                failures++;
                break;
            }
            if (n >= packets.size() || pkt != packets[n]) {
                std::printf("  FAIL  %s: encoded packet %zu differs from the vector\n",
                            v.name, n);
                failures++;
                break;
            }
            n++;
        }
        std::printf("  %-16s %6zu samples, %.3fx\n", v.name, want.size(),
                    wire ? static_cast<double>(want.size() * 2) / wire : 0.0);
    }
}

void TestEdgeCases(const std::string& dir) {
    const std::vector<int16_t> want = LoadPCM(dir + "/edge-cases.pcm");
    const auto packets = LoadPackets(dir + "/edge-cases.uiq");
    uiq::StreamDecoder dec;
    std::vector<int16_t> got;
    int silent = 0, escape = 0, resync = 0;
    bool seen[8] = {false};
    int profiles = 0;
    for (size_t n = 0; n < packets.size(); n++) {
        if (packets[n][4] & uiq::kFlagMetadata) resync++;
        uiq::Header h;
        std::vector<int16_t> s;
        std::string err;
        if (!dec.Decode(packets[n].data(), packets[n].size(), &h, &s, &err)) {
            std::printf("  FAIL  edge-cases packet %zu: %s\n", n, err.c_str());
            failures++;
            return;
        }
        silent += h.silent;
        escape += h.escape;
        if (!seen[h.profile & 7]) { seen[h.profile & 7] = true; profiles++; }
        got.insert(got.end(), s.begin(), s.end());
    }
    Check(got == want, "edge-cases: decode is not lossless");
    Check(silent > 0 && escape > 0 && resync > 1 && profiles > 1,
          "edge-cases: does not cover what it should");
    std::printf("  edge-cases       %d silent, %d escaped, %d resync, %d profiles\n",
                silent, escape, resync, profiles);
}

// The reduced-depth IQ vector (SPEC.md 6.6).
//
// Decode-only, like edge-cases: it is defined by a generator in
// go/ubersdriq/vectors_test.go rather than by encoding a .pcm with fixed
// parameters, and it additionally depends on a shift schedule that is not
// derivable from its samples. Its .pcm holds the RESTORED samples -- what a
// decoder must produce -- not the samples that went into the encoder. This is
// the one vector where those differ.
void TestScaledVector(const std::string& dir) {
    const std::vector<int16_t> want = LoadPCM(dir + "/iq-scaled.pcm");
    const auto packets = LoadPackets(dir + "/iq-scaled.uiq");
    uiq::StreamDecoder dec;
    // A second, independent header decoder, so the shift byte at the front of
    // each scaled body can be read without reaching inside the stream decoder.
    uiq::HeaderDecoder hdr;
    std::vector<int16_t> got;
    int scaled = 0, silent = 0, escape = 0, changes = 0;
    bool shifts[16] = {false};
    int nshifts = 0;
    uint8_t last = 0xff;
    for (size_t n = 0; n < packets.size(); n++) {
        uiq::Header hh;
        size_t off = 0;
        std::string err;
        if (!hdr.Decode(packets[n].data(), packets[n].size(), &hh, &off, &err)) {
            std::printf("  FAIL  iq-scaled packet %zu header: %s\n", n, err.c_str());
            failures++;
            return;
        }
        if (hh.profile == uiq::kProfileIQScaled && !hh.silent && off < packets[n].size()) {
            const uint8_t s = packets[n][off];
            if (s < 16 && !shifts[s]) { shifts[s] = true; nshifts++; }
        }

        uiq::Header h;
        std::vector<int16_t> s;
        if (!dec.Decode(packets[n].data(), packets[n].size(), &h, &s, &err)) {
            std::printf("  FAIL  iq-scaled packet %zu: %s\n", n, err.c_str());
            failures++;
            return;
        }
        if (h.profile == uiq::kProfileIQScaled) scaled++;
        silent += h.silent;
        escape += h.escape;
        if (h.profile != last) { changes++; last = h.profile; }
        got.insert(got.end(), s.begin(), s.end());
    }
    Check(got == want, "iq-scaled: decode does not match the restored samples");
    // The vector is only worth checking against if it still contains every case
    // it claims to: a shift of zero and several non-zero ones, a silent scaled
    // packet, an escaped scaled packet, and the crossings in and out of the
    // lossless profile.
    Check(shifts[0] && nshifts >= 4 && scaled > 0 && silent > 0 && escape > 0 &&
              changes >= 3,
          "iq-scaled: does not cover what it should");
    std::printf("  iq-scaled        %d scaled, %d silent, %d escaped, %d profile changes,"
                " %d shifts\n", scaled, silent, escape, changes, nshifts);
}

// Full-scale white noise cannot be predicted, so the escape must be taken and
// the body must not exceed the samples.
void TestEscape() {
    uint32_t state = 2463534242u;
    auto rnd = [&state]() {
        state ^= state << 13;
        state ^= state >> 17;
        state ^= state << 5;
        return state;
    };
    uiq::Codec enc, dec;
    std::string err;
    Check(uiq::Codec::Create(uiq::kProfileAudio, &enc, &err), "create encoder");
    Check(uiq::Codec::Create(uiq::kProfileAudio, &dec, &err), "create decoder");
    for (int r = 0; r < 20; r++) {
        std::vector<int16_t> s(240);
        for (auto& v : s) v = static_cast<int16_t>(rnd() >> 16);
        std::vector<uint8_t> body;
        bool escape = false;
        Check(enc.Encode(s.data(), s.size(), &body, &escape, &err), "encode noise");
        Check(escape, "white noise should take the escape");
        Check(body.size() <= s.size() * 2, "the body must never expand");
        std::vector<int16_t> back;
        Check(dec.Decode(body.data(), body.size(), s.size(), escape, &back, &err),
              "decode noise");
        Check(back == s, "escape round trip");
    }
    std::printf("  escape           20 packets of full-scale noise\n");
}

// Skipping the body of a silent packet must leave the filters exactly where
// coding the zeros would have.
void TestSilence() {
    std::vector<int16_t> tone(240), zeros(240, 0);
    for (size_t i = 0; i < tone.size(); i++) tone[i] = ((i / 8) % 2) ? 9000 : -9000;
    uiq::Codec coded, skipped;
    std::string err;
    uiq::Codec::Create(uiq::kProfileAudio, &coded, &err);
    uiq::Codec::Create(uiq::kProfileAudio, &skipped, &err);
    std::vector<uint8_t> a, b;
    bool e = false;
    for (int i = 0; i < 5; i++) {
        coded.Encode(tone.data(), tone.size(), &a, &e, &err);
        skipped.Encode(tone.data(), tone.size(), &b, &e, &err);
    }
    for (int i = 0; i < 10; i++) {
        coded.Encode(zeros.data(), zeros.size(), &a, &e, &err);
        skipped.AdvanceSilence(zeros.size(), &err);
    }
    coded.Encode(tone.data(), tone.size(), &a, &e, &err);
    skipped.Encode(tone.data(), tone.size(), &b, &e, &err);
    Check(a == b, "advancing over silence left the filters in a different state");
    std::printf("  silence          filters stay in step across a silent run\n");
}

// Ids 0, 1 and 2 are the defined profiles; 3 to 7 are unassigned and must be a
// hard error rather than a fallback, because decoding with the wrong predictor
// does not fail, it returns plausible-sounding noise.
void TestUnknownProfile() {
    for (uint8_t id : {3, 4, 7}) {
        uiq::Codec c;
        std::string err;
        Check(!uiq::Codec::Create(id, &c, &err), "an unknown profile was accepted");
    }
    uiq::Codec c;
    std::string err;
    Check(uiq::Codec::Create(uiq::kProfileIQScaled, &c, &err),
          "the scaled profile was refused");
    Check(c.scaled(), "the scaled profile does not report a scaled body");
    std::printf("  profiles         unknown ids refused, profile 2 accepted\n");
}

// The reduced-depth profile: requantise, code, restore, and check the samples
// that come out match what the quantiser produced.
//
// The shift is chosen here rather than derived from the samples, which is the
// point: choosing one is sender policy and nothing on the wire depends on it.
void TestScaled() {
    for (unsigned shift : {0u, 1u, 4u, 10u, 15u}) {
        uiq::StreamEncoder enc;
        uiq::StreamDecoder dec;
        std::vector<int16_t> s(480);
        for (size_t i = 0; i < s.size(); i++) {
            s[i] = static_cast<int16_t>((i % 2 ? 1 : -1) *
                                        (12000 + static_cast<int>(i * 37 % 20000)));
        }
        s[0] = 32767;
        s[1] = -32768;
        std::vector<int16_t> want = s;
        uiq::Quantise(want.data(), want.size(), shift);
        uiq::Restore(want.data(), want.size(), shift);

        std::vector<uint8_t> pkt;
        std::string err;
        Check(enc.EncodeScaled(s.data(), s.size(), 1700000000000000000ull, 384000, 2,
                               -999.0f, -999.0f, shift, &pkt, &err),
              "encode scaled");
        uiq::Header h;
        std::vector<int16_t> got;
        Check(dec.Decode(pkt.data(), pkt.size(), &h, &got, &err), "decode scaled");
        Check(h.profile == uiq::kProfileIQScaled, "scaled packet declares profile 2");
        Check(got == want, "the restored samples are not the quantised ones");

        // A silent scaled packet carries no shift byte, so its packet ends
        // where its header ended.
        std::vector<int16_t> zeros(480, 0);
        Check(enc.EncodeScaled(zeros.data(), zeros.size(), 1700000000000000000ull,
                               384000, 2, -999.0f, -999.0f, shift, &pkt, &err),
              "encode silent scaled");
        Check(dec.Decode(pkt.data(), pkt.size(), &h, &got, &err), "decode silent scaled");
        Check(h.silent && got == zeros, "silent scaled packet");
    }

    // A shift past 15 is a corrupt packet, not a stream to render.
    uiq::StreamEncoder enc;
    uiq::StreamDecoder dec;
    std::vector<int16_t> s(480, 1000);
    std::vector<uint8_t> pkt;
    std::string err;
    Check(enc.EncodeScaled(s.data(), s.size(), 1700000000000000000ull, 384000, 2,
                           -999.0f, -999.0f, 3, &pkt, &err), "encode scaled");
    uiq::Header h;
    std::vector<int16_t> got;
    // The shift is the first byte of the body, so an independent header decoder
    // finds it without reaching inside the stream decoder.
    uiq::HeaderDecoder hdr;
    size_t off = 0;
    Check(hdr.Decode(pkt.data(), pkt.size(), &h, &off, &err), "header of a scaled packet");
    Check(off < pkt.size() && pkt[off] == 3, "the shift is not the first body byte");
    pkt[off] = 16;
    Check(!dec.Decode(pkt.data(), pkt.size(), &h, &got, &err),
          "a shift above 15 was accepted");
    std::printf("  scaled           5 shifts, silence, and an out-of-range shift\n");
}

}  // namespace

int main(int argc, char** argv) {
    const std::string dir = argc > 1 ? argv[1] : "testdata";
    std::printf("conformance against %s\n", dir.c_str());
    TestVectors(dir);
    TestEdgeCases(dir);
    TestScaledVector(dir);
    TestEscape();
    TestSilence();
    TestUnknownProfile();
    TestScaled();
    if (failures) {
        std::printf("%d failure(s)\n", failures);
        return 1;
    }
    std::printf("all checks passed\n");
    return 0;
}
