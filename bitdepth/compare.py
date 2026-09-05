#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

"""Compare jt9 decode sets between bit depths.

    compare.py REFERENCE.decodes ARM.decodes [ARM.decodes ...]

Each input holds jt9 FT8 output with a leading `period NN` line before each
period's decodes, as run.sh writes them.

A decode is identified by (period, message), not by the SNR and dt columns:
those move by a dB either way and are not what "did it decode" means. The
trailing a1/a2/a3 flag marks a decode that needed a priori information; it is
stripped for matching and reported separately, because a message that only
comes back with AP has degraded even though it still appears.
"""

import re
import sys
from collections import Counter

AP = re.compile(r"\s+a[0-9]\s*$")


def load(path):
    """-> {(period, message): (snr, needed_ap)}, allowing repeats per period."""
    out = Counter()
    snrs, ap = {}, {}
    period = -1
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            if line.startswith("period "):
                period = int(line.split()[1])
                continue
            if not line.strip():
                continue
            # 000000  -8  0.2 2641 ~  CQ SQ8AA KN19
            parts = line.split(None, 4)
            if len(parts) < 5:
                continue
            snr = int(parts[1])
            msg = parts[4].strip()
            needed_ap = bool(AP.search(msg))
            msg = AP.sub("", msg).strip()
            key = (period, msg)
            out[key] += 1
            snrs.setdefault(key, snr)
            ap[key] = needed_ap or ap.get(key, False)
    return out, snrs, ap


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__)
        return 2

    ref_path = sys.argv[1]
    ref, ref_snr, _ = load(ref_path)
    ref_total = sum(ref.values())

    print(f"{'arm':10s} {'decodes':>8s} {'of ref':>8s} {'lost':>6s} "
          f"{'gained':>7s} {'AP-only':>8s}")
    print(f"{'16-bit':10s} {ref_total:8d} {'100.0%':>8s} {'-':>6s} {'-':>7s} {'-':>8s}")

    details = []
    for path in sys.argv[2:]:
        arm, arm_snr, arm_ap = load(path)
        total = sum(arm.values())
        lost = ref - arm          # Counter subtraction keeps positive counts
        gained = arm - ref
        # Present in both, but the arm needed AP where the reference did not.
        ap_only = [k for k in (ref & arm) if arm_ap.get(k) and not _ref_ap.get(k)]
        name = path.split("/")[-1].replace(".decodes", "")
        print(f"{name:10s} {total:8d} {100.0*total/ref_total:7.1f}% "
              f"{sum(lost.values()):6d} {sum(gained.values()):7d} {len(ap_only):8d}")
        details.append((name, lost, gained, ap_only, ref_snr, arm_snr))

    # Per-decode SNR agreement. A matching count proves the messages came
    # back; this proves they came back at the same strength, which is the
    # sharper test -- a decode scraping through 3 dB weaker has degraded even
    # though it still counts.
    for name, lost, gained, ap_only, rsnr, asnr in details:
        common = sorted(set(rsnr) & set(asnr))
        if common:
            d = [asnr[k] - rsnr[k] for k in common]
            hist = Counter(d)
            print(f"\n{name}: SNR agreement over {len(common)} matched decodes")
            print(f"  mean {sum(d)/len(d):+.2f} dB, worst {min(d):+d}/{max(d):+d} dB")
            print("  " + "  ".join(f"{k:+d} dB x{v}" for k, v in sorted(hist.items())))

    for name, lost, gained, ap_only, rsnr, asnr in details:
        if not (lost or gained or ap_only):
            continue
        print(f"\n{name}:")
        for (p, m), n in sorted(lost.items(), key=lambda kv: rsnr.get(kv[0], 0)):
            print(f"  lost     period {p}  {rsnr.get((p, m), 0):+4d} dB  {m}"
                  + (f"  (x{n})" if n > 1 else ""))
        for (p, m), n in sorted(gained.items(), key=lambda kv: asnr.get(kv[0], 0)):
            print(f"  gained   period {p}  {asnr.get((p, m), 0):+4d} dB  {m}"
                  + (f"  (x{n})" if n > 1 else ""))
        for (p, m) in sorted(ap_only, key=lambda k: rsnr.get(k, 0)):
            print(f"  AP only  period {p}  {rsnr.get((p, m), 0):+4d} dB  {m}")
    return 0


if __name__ == "__main__":
    # The reference's own AP flags, needed to tell a newly-AP decode from one
    # that always was.
    _ref_ap = {}
    if len(sys.argv) > 1:
        _, _, _ref_ap = load(sys.argv[1])
    sys.exit(main())
