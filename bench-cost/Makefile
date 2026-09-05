# Per-packet cost benchmark for SPEC.md section 10.2.
#
#   make            -- the conformant benchmark
#   make bench-noleak -- the same, against a tree with the tap leakage of
#                        section 6.2.3 mechanically removed, to isolate what
#                        the leak costs. Its output is NOT conformant.
CXXFLAGS ?= -O2 -std=c++17
LDLIBS   ?= -lm

all: bench bench-noleak sessions

bench: bench.cpp ../cpp/ubersdr_iq.hpp
	$(CXX) $(CXXFLAGS) -I../cpp bench.cpp -o $@ $(LDLIBS)

# The leak-free header is generated, never edited, so it cannot drift from the
# real one: the three substitutions delete exactly the leak term from each of
# the three tap updates and touch nothing else.
noleak/ubersdr_iq.hpp: ../cpp/ubersdr_iq.hpp
	@mkdir -p noleak
	sed -e 's/ - Leak(owr, kLeakShiftComplex)//' \
	    -e 's/ - Leak(owi, kLeakShiftComplex)//' \
	    -e 's/ - Leak(ow, kLeakShiftReal)//' \
	    -e 's/^inline int64_t Leak(/[[maybe_unused]] inline int64_t Leak(/' \
	    $< > $@
	@test `grep -c 'Leak(o' $@` -eq 0 || { echo "leak removal incomplete"; exit 1; }

bench-noleak: bench.cpp noleak/ubersdr_iq.hpp
	$(CXX) $(CXXFLAGS) -Inoleak bench.cpp -o $@ $(LDLIBS)

clean:
	rm -rf bench bench-noleak sessions noleak

.PHONY: all clean

sessions: sessions.cpp ../cpp/ubersdr_iq.hpp
	$(CXX) $(CXXFLAGS) -I../cpp -pthread sessions.cpp -o $@ $(LDLIBS)
