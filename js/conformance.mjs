// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Nathan Higgins

// Conformance test for the JavaScript decoder.
//
//     node js/conformance.mjs [testdata-dir]
//
// This is a decoder, so only one direction can be checked: every .uiq stream
// must reconstruct its .pcm samples exactly. The Go, Python and C++
// implementations additionally check that their encoders reproduce the vectors
// byte for byte.

import { readFileSync } from 'node:fs';
import { StreamDecoder, readStream, FLAG_METADATA } from './ubersdr-iq.mjs';

const dir = process.argv[2] ?? new URL('../testdata', import.meta.url).pathname;

const vectors = [
    ['audio-cw-12k', 12000, 1],
    ['audio-usb-12k', 12000, 1],
    ['iq-12k', 12000, 2],
    ['iq-384k', 384000, 2],
    ['iq-384k-mw', 384000, 2],
    ['rice-63', 96000, 2],
    ['edge-cases', 0, 0],
    ['iq-scaled', 0, 0],
];

// The two generated vectors change their parameters mid-stream, so a rate of
// zero above means "do not check". Their .pcm holds what a decoder must
// produce -- for iq-scaled, the samples after the shift has been restored --
// and what they cover is reported rather than a compression ratio, which for a
// lossy stream would not mean anything comparable to the others.
const MIXED = new Set(['edge-cases', 'iq-scaled']);

let failures = 0;

function loadPCM(name) {
    const b = readFileSync(`${dir}/${name}.pcm`);
    return new Int16Array(b.buffer, b.byteOffset, b.byteLength / 2);
}

for (const [name, rate, channels] of vectors) {
    const want = loadPCM(name);
    const file = readFileSync(`${dir}/${name}.uiq`);
    const dec = new StreamDecoder();
    const got = new Int16Array(want.length);
    let at = 0, wire = 0, n = 0;
    let silent = 0, escape = 0, resync = 0;
    const profiles = new Set();
    const shifts = new Set();

    for (const pkt of readStream(file)) {
        if (pkt[4] & FLAG_METADATA) resync++;
        const r = dec.decode(pkt);
        if (!r) {
            console.log(`  FAIL  ${name}: packet ${n} did not decode`);
            failures++;
            break;
        }
        if (rate && (r.sampleRate !== rate || r.channels !== channels)) {
            console.log(`  FAIL  ${name}: packet ${n} says ${r.sampleRate} Hz / ${r.channels} ch`);
            failures++;
            break;
        }
        if (r.silent) silent++;
        if (r.escape) escape++;
        profiles.add(r.profile);
        if (!r.silent && r.profile === 2) shifts.add(r.shift);
        got.set(r.samples, at);
        at += r.samples.length;
        wire += pkt.length;
        n++;
    }

    if (at !== want.length) {
        console.log(`  FAIL  ${name}: decoded ${at} samples, expected ${want.length}`);
        failures++;
        continue;
    }
    let bad = -1;
    for (let i = 0; i < want.length; i++) {
        if (got[i] !== want[i]) { bad = i; break; }
    }
    if (bad >= 0) {
        console.log(`  FAIL  ${name}: sample ${bad} is ${got[bad]}, expected ${want[bad]}`);
        failures++;
        continue;
    }
    let extra;
    if (name === 'iq-scaled') {
        extra = `${silent} silent, ${escape} escaped, profiles ${[...profiles].sort()},`
            + ` shifts ${[...shifts].sort((a, b) => a - b)}`;
    } else if (MIXED.has(name)) {
        extra = `${silent} silent, ${escape} escaped, ${resync} resync, profiles ${[...profiles].sort()}`;
    } else {
        extra = `${(want.length * 2 / wire).toFixed(3)}x`;
    }
    console.log(`  ${name.padEnd(16)} ${String(want.length).padStart(6)} samples, ${extra}`);
}

if (failures) {
    console.log(`${failures} failure(s)`);
    process.exit(1);
}
console.log('all checks passed');
