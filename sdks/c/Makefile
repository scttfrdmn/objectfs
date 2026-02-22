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

GOENV   := GONOSUMDB="github.com/scttfrdmn/*" GOPRIVATE="github.com/scttfrdmn/*"
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
clean:
	rm -f $(LIB) libobjectfs.h tests/test_basic
