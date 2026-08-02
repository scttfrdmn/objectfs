// Package difftest is a differential-testing oracle for ObjectFS's file semantics.
//
// It runs one operation sequence against two implementations of the same interface — ObjectFS, and
// the local operating-system filesystem — and asserts they agree: on what each read returns, on the
// size each reports, and on the bytes that end up durable. The local filesystem is not a model of
// POSIX; it is POSIX, which is the point. A hand-written model of the expected result can encode the
// same misunderstanding as the implementation, and if it does, the test passes and says nothing.
//
// # Why this exists
//
// The v0.10.0 audit found roughly forty-five defects that 32,680 lines of tests across 90 files had
// all missed. The write-path ones were not subtle in effect — appending one byte to a 1 MiB file left
// a 1-byte object — but every one of them was invisible to a unit test, because each layer was tested
// against a mock of its neighbor and the mock restated what the caller believed. The bug lived in
// the disagreement between layers, which is exactly what a mock removes.
//
// A differential oracle cannot be fooled that way, because the reference is not written by the same
// person who wrote the implementation, does not share its assumptions, and has no opinion about
// ObjectFS's internals. It answers one question — "is this what a filesystem does?" — and it answers
// it about the composed system rather than about a layer.
//
// These are the sequences that were proven to lose data in v0.10.0, and each is caught here by
// nothing cheaper than running it against a real filesystem and comparing:
//
//	pwrite(f,"AAAA",0); pwrite(f,"BBBB",4)   → object is "BBBB", not "AAAABBBB"
//	pwrite(f,"X",1048575) on a 1 MiB file    → object becomes 1 byte
//	pwrite(f,hdr,0); pwrite(f,page,65536)    → EIO
//	echo NEW > f  (over "OLD")               → file still reads "OLD"
//	write, then read the same offset         → returns pre-write bytes
//
// # Scope
//
// The oracle compares file *semantics*: content, size, and durability. It does not compare errno
// values, permissions, timestamps, or anything about directories. An ObjectFS implementation is free
// to refuse an operation the local filesystem accepts — but it must refuse it loudly, and a refusal
// is never equivalent to a silent wrong answer. [Compare] treats an unexpected error as a failure
// with the same weight as wrong bytes, because a filesystem that returns EIO for a legitimate write
// is broken, just less dangerously than one that reports success and drops the data.
//
// # Using it
//
// [Compare] returns a [Divergence] describing the first disagreement and the [Program] that produced
// it, rendered as pasteable Go. [Shrink] reduces a failing program to a minimal one, which is what
// makes fuzz output usable: a 200-operation counterexample says nothing a human can act on, and the
// three-operation one hiding inside it usually says everything.
//
// The two implementations that ship with the package are [Local], the reference, and [Legacy], the
// write path v0.10.0 shipped. Legacy exists so the harness has demonstrated teeth on real defects
// before it is trusted to guard against new ones, and it is deleted along with that write path.
package difftest
