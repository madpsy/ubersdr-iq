# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Nathan Higgins

CXX ?= c++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra

all: conformance example

conformance: conformance.cpp ubersdr_iq.hpp
	$(CXX) $(CXXFLAGS) -o $@ conformance.cpp

example: example.cpp ubersdr_iq.hpp
	$(CXX) $(CXXFLAGS) -o $@ example.cpp

check: conformance
	./conformance ../testdata

clean:
	rm -f conformance example

.PHONY: all check clean
