# ObjectFS C SDK — Makefile
#
# Targets:
#   make build       build the shared library
#   make test        build + run all tests
#   make test-c      run the C test binary only
#   make test-python run the Python ctypes smoke test only
#   make clean       remove build artefacts

UNAME := $(shell uname)

ifeq ($(UNAME), Darwin)
    LIB      := libobjectfs.dylib
    RPATH    := -Wl,-rpath,$(CURDIR)
    LIB_PATH := DYLD_LIBRARY_PATH=$(CURDIR)
else
    LIB      := libobjectfs.so
    RPATH    := -Wl,-rpath,'$$ORIGIN'
    LIB_PATH := LD_LIBRARY_PATH=$(CURDIR)
endif

GOFLAGS := -buildmode=c-shared

CC      ?= cc
CFLAGS  := -I$(CURDIR) -L$(CURDIR) -lobjectfs -Wall -Wextra $(RPATH)

.PHONY: all build test test-c test-python clean

all: build

## build — produce libobjectfs.{so,dylib} + libobjectfs.h
build: $(LIB)

$(LIB): main.go objectfs_types.h objectfs.h
	$(GOENV) go build $(GOFLAGS) -o $(LIB) .

## test — build and run all tests
test: test-c test-python

## test-c — compile and run the C test binary
test-c: $(LIB) tests/test_basic.c objectfs.h
	$(CC) -o tests/test_basic tests/test_basic.c $(CFLAGS)
	$(LIB_PATH) ./tests/test_basic

## test-python — run the Python ctypes smoke test
test-python: $(LIB)
	$(LIB_PATH) python3 tests/test_smoke.py

## clean — remove build artefacts
#
# libobjectfs.h is deliberately not removed. The shared library is gitignored (.gitignore:5-6)
# but the cgo-generated header is checked in, because README.md lists it as the reference copy
# of the declarations for a reader who has not built anything. Deleting it here left `make clean`
# reporting a deleted tracked file in `git status`.
clean:
	rm -f $(LIB) tests/test_basic
