# Requires libvolk and libzstd development packages, and the ubersdr-iq C++
# reference header one directory up.
CXXFLAGS ?= -O2 -std=c++17 -I../cpp
LDLIBS   ?= -lvolk -lzstd -lm

bench: bench.cpp ../cpp/ubersdr_iq.hpp
	$(CXX) $(CXXFLAGS) bench.cpp -o $@ $(LDLIBS)

clean:
	rm -f bench rec_*.pcm iq-384k_*.pcm iq-384k-mw_*.pcm smoke_*.pcm sh48-*.bin sh-iq-384k*.bin

.PHONY: clean
