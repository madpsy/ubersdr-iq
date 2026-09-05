import numpy as np, sys

import os
SRC=sys.argv[1]; RATE=int(sys.argv[2]); PFX=sys.argv[3]
def load(p):
    a=np.fromfile(p,dtype='<i2').astype(np.float64)
    return a[0::2]+1j*a[1::2]

orig=load(SRC)
N=4096
w=np.hanning(N)
cg=(w**2).sum()

def psd(x):
    n=len(x)//N
    x=x[:n*N].reshape(n,N)*w
    S=np.fft.fftshift(np.abs(np.fft.fft(x,axis=1))**2,axes=1).mean(axis=0)
    # dBFS/bin, referenced to full-scale int16
    return 10*np.log10(S/(cg*32768.0**2)+1e-300)

f=(np.arange(N)-N//2)*RATE/N
P0=psd(orig)

print(f"capture: {SRC}, {len(orig)} frames @ {RATE} Hz")
print(f"band occupancy: peak {P0.max():.1f} dBFS/bin, median (noise floor) {np.median(P0):.1f} dBFS/bin")
print(f"peak-to-noise-floor: {P0.max()-np.median(P0):.1f} dB\n")

modes=[("SDR++ int16 (default)",PFX+"int16_default.pcm"),
       ("SDR++ int16, peak fixed",PFX+"int16_peak_fixed.pcm"),
       ("SDR++ int8",PFX+"int8.pcm"),
       ("SDR++ int8, peak fixed",PFX+"int8_peak_fixed.pcm")]
# Further arms arrive as LABEL:PATH arguments. The reduced-depth rows come in
# that way so they are scored on exactly the axes the SDR++ lossy rows are.
for a in sys.argv[4:]:
    lab,_,path=a.partition(":")
    modes.append((lab,path))

print(f"{'mode':<26} {'err floor':>11} {'margin':>10} {'floor rise':>11} {'headroom':>10}")
nf0=np.median(P0)
for name,path in modes:
    if not os.path.exists(path):
        print(f"{name:<26} {'not run':>11}")
        continue
    r=load(path)
    n=min(len(r),len(orig))
    e=r[:n]-orig[:n]
    if not np.any(e):
        print(f"{name:<26} {'—':>11} {'—':>10} {'0.00 dB':>11} {'exact':>10}")
        continue
    Pe=psd(e)
    nfe=np.median(Pe)
    combined=10*np.log10(10**(nf0/10)+10**(nfe/10))
    print(f"{name:<26} {nfe:>8.1f} dB {nf0-nfe:>7.1f} dB {combined-nf0:>8.2f} dB {P0.max()-nfe:>7.1f} dB")

print("\n'err floor'      median PSD of the error signal, dBFS per 4096-bin")
print("'margin'         how far the error floor sits below the band's own noise floor.")
print("                 The quantity a reduced-depth stream is asked for; the SDR++")
print("                 modes hold no margin and are put on the same axis for comparison.")
print("'floor rise'     how much the band's own noise floor goes up once the error is added")
print("'headroom'       strongest signal in the band above the error floor -- the")
print("                 usable dynamic range that survives the mode")
