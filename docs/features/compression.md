# Transparent compression

ObjectFS can compress each object as it is written and decompress it as it is read, so that
applications see ordinary bytes and S3 stores fewer of them. It is **off by default**, and this page
exists because turning it on costs you four things that are not obvious from the configuration key.

```yaml
write_buffer:
  compression:
    enabled: false        # the default
    min_size: 4KB         # smaller objects are stored as-is
    algorithm: zstd       # none, zstd, lz4, gzip
    level: 3              # zstd 0-22, gzip 0-9; 0 selects the codec's default
```

The short version:

- **Enable it** for text-like data — logs, CSV, JSON, uncompressed FASTA/VCF, source trees — that
  you read whole, in a bucket whose only reader is ObjectFS.
- **Do not enable it** for data you read randomly, for data that arrived already compressed (which
  is most research data), or for a bucket that anything else reads.
- **Prefer compression inside the file format** — Parquet column codecs, BGZF, HDF5 filters — wherever
  the format offers it. That is a strictly better version of what this feature does.

---

## What it costs

### 1. A compressed object is not readable by anything but ObjectFS

This is the most important thing on this page, and the one that surprises people.

An object ObjectFS compresses is stored as a compression frame with a `Content-Encoding` header. Every
other S3 client hands you **the compressed bytes, with a successful exit status**. There is no error to
notice.

Measured against `s3://objectfs-test-scttfrdmn` in `us-west-2` on 2026-08-02, using a 277,793-byte
CSV stored the way ObjectFS stores it (zstd level 3 body, `Content-Encoding: zstd`, and an
`objectfs-original-size` metadata key), read back four ways:

| reader | exit status | bytes written to disk | what the file was |
|---|---|---|---|
| `aws s3 cp` (CLI 2.36.14) | 0 | 26,570 | `Zstandard compressed data (v0.8+)` |
| boto3 1.42.30 `get_object` | no exception | 26,570 | zstd frame (`28 b5 2f fd`) |
| `curl --compressed` | 61 | — | `Unrecognized content encoding type` |
| ObjectFS | 0 | 277,793 | the CSV |

So a colleague who runs `aws s3 cp s3://bucket/sample.csv .` gets a file named `sample.csv` that is
not a CSV, and nothing tells them. The failure surfaces later, somewhere else, in a form nobody traces
back to this setting — which is why it usually surfaces during a migration or an incident.

The implicit guarantee that makes S3-backed storage attractive — *my data is just objects in a bucket,
I can always get at it* — is void for compressed objects. Enable compression only if every consumer of
the bucket goes through ObjectFS, or is prepared to decode the encoding itself.

**`gzip` is the one partial exception, and only over HTTP.** Because gzip is a registered HTTP content
coding and zstd is not, a client that negotiates `Accept-Encoding` decodes a gzip object and fails on a
zstd one. In the same measurement, `curl --compressed` returned the full 277,793-byte CSV for the gzip
object and refused the zstd object outright. But `aws s3 cp` and boto3 decode **neither** — both wrote
the raw 94,068-byte gzip frame to disk with a successful status. So gzip buys back browser and
`curl`-style readability, not S3-tool readability, in exchange for compressing worse and slower than
zstd at every level. If outside readers are the reason you are asking, the answer is usually to leave
compression off rather than to pick gzip.

### 2. A partial read of a compressed object transfers the whole object

A zstd, gzip, or lz4 frame must be decoded from its start, so a range of the decoded content cannot be
served from a range of the stored bytes. ObjectFS fetches the entire stored object and slices after
decoding. That is correct, and it is expensive.

Measured against the same bucket and region on 2026-08-02, with a 4 KiB read at offset 1 MiB. The
payloads are half low-entropy text and half `/dev/urandom` in alternating 4 KiB runs, which zstd level 3
takes to 44.8% — deliberately unflattering to compression, so that the stored object is large enough for
the cost to be real:

| object size | stored size | bytes moved for a 4 KiB read | amplification |
|---|---|---|---|
| 16 MiB | 7,518,464 | 7,518,464 | 1,836× |
| 64 MiB | 30,082,049 | 30,082,049 | 7,344× |
| 256 MiB | 120,342,259 | 120,342,259 | 29,380× |

The amplification factor is just `stored size ÷ read size`, so it grows without bound: a 4 KiB read of a
compressed 10 GiB object transfers the whole compressed body. An uncompressed object of any of those
sizes moves exactly 4,096 bytes, because the read is served by a `Range` request.

Bytes are the figure to plan against; wall time depends on where you are. From an off-region macOS
client, three repetitions each, the compressed reads took a median 0.43 s / 0.71 s / 1.78 s against
0.14 s for the ranged reads — 3.0× / 5.0× / 12.3×. The v0.10.0 audit measured the same shape at
15.6× / 43× / 216.5× on a different day, link, and payload. Both are true, and neither is a property of
ObjectFS. The byte counts above are, which is why the regression tests in
`internal/storage/s3/read_amplification_test.go` assert bytes transferred and not latency.

For a sequential workload — `cat`, `cp`, a whole-file read, a tar extract — none of this matters; the
whole object was going to cross the wire anyway. For random access — a SQLite or DuckDB file, an indexed
BAM, mmap'd data, an HDF5 file read by chunk, a Zarr store — it is disqualifying.

[#185](https://github.com/scttfrdmn/objectfs/issues/185) tracks seekable zstd framing, which would fix
this. Until it lands: **do not enable compression on objects you read randomly.**

### 3. Compressed objects give up parallel range reads

Parallel reads fan a large read out into concurrent range GETs above
`performance.parallel_read.threshold`. A compressed object cannot use that path — the frame has to be
decoded from its start, so there is no set of independent ranges to assemble — so a large read of a
compressed object is one whole-object GET. This is cost #2 measured in requests rather than in bytes.

Only compressed objects pay it. Through v0.10.0 the whole bucket did: the gate asked whether a codec
was *configured* rather than whether the object was *encoded*, so on a mount with compression enabled
a large read of an object that had never been compressed lost the fan-out too — which meant the
objects that gain nothing from compression were the same ones losing parallel reads because of it.
Fixed in [#228](https://github.com/scttfrdmn/objectfs/issues/228), which decides from the object and
is pinned by `TestFanOutIsDecidedByTheObjectNotTheConfig` running the same read with
`compression.enabled` both ways. That gate was the residue of audit finding C4: the
whole-object-versus-ranged decision had been moved onto the object and this one, a line above it,
had not.

### 4. On some tiers, compression can save nothing at all

S3 applies a **minimum billable object size** on three storage classes, against the size actually
stored. Compressing below that floor changes the invoice by zero. From
[AWS's storage class comparison](https://docs.aws.amazon.com/AmazonS3/latest/userguide/storage-class-intro.html#sc-compare)
and [S3 pricing](https://aws.amazon.com/s3/pricing/):

| storage class | minimum billable object size | minimum storage duration |
|---|---|---|
| `STANDARD` | none | none |
| `STANDARD_IA` | 128 KB | 30 days |
| `ONEZONE_IA` | 128 KB | 30 days |
| `GLACIER_IR` | 128 KB | 90 days |
| `INTELLIGENT_TIERING` | none | none |
| `GLACIER` | none — but see below | 90 days |
| `DEEP_ARCHIVE` | none — but see below | 180 days |

On the three classes with a 128 KB floor, compressing a 100 KB object to 40 KB saves **nothing**: both
are billed as 128 KB. You paid CPU on the write, you pay it on every read, and the invoice is
unchanged. Compression reduces storage cost on those tiers only when the **compressed** size still
exceeds 128 KB.

`GLACIER` and `DEEP_ARCHIVE` behave differently, and the difference runs the other way. They have no
billable floor; instead AWS adds **40 KB of metadata per archived object** — 32 KB at the archive rate
plus 8 KB at the S3 Standard rate. That is an addition, not a floor, so compression *does* reduce the
bill there. What it cannot reduce is the surcharge, which dominates for small objects. Storing a 10 KB
object on `DEEP_ARCHIVE` at the `us-east-1` list rates in `internal/awsrates` — $0.00099/GB-month
archive, $0.023/GB-month Standard — costs about **23× what the payload alone would**, and 82% of that
total is the 8 KB billed at Standard rates. Compressing that object to 5 KB changes the bill by 2.2%.
Many small objects are expensive on the archive tiers whatever you do with them; compression is not the
lever.

`INTELLIGENT_TIERING` has no billable minimum either. What 128 KB governs there is *monitoring*:
objects below it are not auto-tiered, stay in the Frequent Access tier, and are not charged the
monitoring fee.

Three of those rows used to contradict what `internal/storage/s3/tiers.go` encoded: it carried a 40 KB
minimum for `GLACIER` and `DEEP_ARCHIVE` and a 128 KB minimum for `INTELLIGENT_TIERING`, none of which
AWS publishes. Corrected in [#229](https://github.com/scttfrdmn/objectfs/issues/229), which gave the
40 KB and the 128 KB fields named for what they are — `PerObjectOverheadBytes` and
`MonitoringEligibilityBytes` — and pinned all eight classes against the AWS pages linked above in
`TestTierSizeThresholdsMatchWhatAWSPublishes`. The minimum is also no longer a *write gate*: through
v0.11.0 a write below a tier's minimum was refused, which is not what a billing floor is, and it made
`mkdir` and `touch` fail on all three of those classes because both create a zero-byte object. It
warns now, naming the written size and the size that will be billed
([#154](https://github.com/scttfrdmn/objectfs/issues/154)). The AWS pages are the authority, not this
page and not that table.

---

## What actually compresses

Most research data does not, because it arrived compressed. `internal/compression/analyzer.go` already
recognizes these by magic bytes, and every one of them is at or near its entropy limit — compressing it
again spends CPU to add a few bytes:

gzip · zstd · bzip2 · lz4 · xz · zip · PNG · JPEG · GIF · WebP · MP4/MOV/M4A · Matroska · Ogg · MP3 · PDF

The formats that matter for research computing are worse than that list suggests, because their
compression is *internal* and so invisible to a magic-byte check on the container:

- **BAM, CRAM** — BGZF, which is gzip per block
- **Parquet, ORC** — per-column codecs, usually snappy or zstd
- **Zarr, HDF5/NetCDF-4** — per-chunk filters, frequently zlib or blosc
- **`.tar.gz`, `.tar.zst`, `.tar.bz2`** — compressed by construction

What does compress:

plain text · CSV/TSV · XML · JSON · YAML · source code · logs · uncompressed FASTA/FASTQ · uncompressed
VCF · plain `.tar` · NetCDF-3 · SAM (the uncompressed form of BAM)

How much depends entirely on how repetitive your data is, so measure rather than budget from a rule of
thumb. For calibration, zstd level 3 on this machine: 4.0× on 2.4 MB of Go source concatenated from this
repository, 8.1× on 4.1 MB of generated JSON records, 15.0× on 3.2 MB of generated CSV. The synthetic
files are more repetitive than real data usually is; the source tree is the realistic end of that range.
`zstd -3 -k yourfile` on a representative sample answers the question directly and costs nothing.

For a typical genomics or imaging bucket, compression saves close to nothing. Check before enabling:
list the extensions in the bucket and see what fraction is on the second list rather than the first.

ObjectFS does not yet make this check for you — it compresses whatever it is handed above `min_size`,
including data it cannot help. The analyzer that would skip those formats exists and has no caller;
[#184](https://github.com/scttfrdmn/objectfs/issues/184) wires it up.

---

## Project-level compression versus "S3-native" compression

There is no S3-native transparent compression. S3 stores the bytes you give it. What the phrase usually
means is one of three things:

**`Content-Encoding` set by whatever uploaded the object.** Identical tradeoffs to this feature, minus
the transparency — you get the compressed bytes unless your client decodes them. This is the same
mechanism ObjectFS uses, which is why an ObjectFS-compressed object is readable by a `Content-Encoding`-
aware HTTP client and not by `aws s3 cp`.

**S3 Select, Athena, or a query engine reading compressed objects.** Decompression at query time for
specific formats. Useful, but it is not general storage compression and it does not help a filesystem.

**Compression inside the file format** — Parquet column codecs, BGZF, HDF5/Zarr chunk filters. **This is
usually the right answer**, and it is better than ObjectFS-level compression on every axis that matters
here:

| | format-internal | ObjectFS-level |
|---|---|---|
| ranged read | seekable by design; fetches only the blocks it needs | fetches the whole object |
| readable by other tools | yes — every reader of the format understands it | no |
| tuned per column or chunk | yes | one codec for the whole object |
| requires ObjectFS | no | yes |

So the honest guidance: prefer format-internal compression wherever the format supports it.
ObjectFS-level compression is for data whose format has no compression of its own and that you read
whole — which, once you exclude everything above, is mostly text.

---

## What compression does not buy

The question this page grew out of was: *project-level compression saves bandwidth and end-to-end
latency — what else does it provide?* Less than you would expect.

- **It does not reduce request counts.** The same number of GETs and PUTs, at the same per-request price.
  On the three tiers with a 128 KB billable floor it may not reduce storage cost either.
- **It does not improve durability or integrity.** ObjectFS records a SHA-256 of the *uncompressed*
  content on every write and verifies it on every whole-object read, compressed or not. Compression
  neither adds nor subtracts from that.
- **It does not speed up writes.** It costs CPU on the write path, and until [#184](https://github.com/scttfrdmn/objectfs/issues/184)
  lands it costs that CPU even on data that cannot compress.
- **It does not make small objects cheap.** See the billing minimums above.
- **It adds a failure mode.** An object whose stored encoding cannot be decoded fails closed with an
  integrity error. That is the correct behavior — v0.10.0 returned the raw compressed frame with exit
  status 0, which is audit finding C2 — but it is a way for a read to fail that an uncompressed object
  does not have. Changing `algorithm` no longer reaches it ([#230](https://github.com/scttfrdmn/objectfs/issues/230)),
  so what is left is an object whose `Content-Encoding` names a coding ObjectFS does not implement, or
  one whose header was stripped after the write — a `CopyObject` or a tier transition does not carry it.

---

## If you do enable it

```yaml
write_buffer:
  compression:
    enabled: true
    algorithm: zstd       # gzip only if HTTP clients must read the objects; see above
    level: 3              # 3 is a good ratio-per-CPU point; above ~9 the returns are small
    min_size: 4KB         # below this, framing overhead and the tier floor dominate
```

`min_size` deserves a thought rather than the default. On `STANDARD` there is no billing floor, so 4 KB
is reasonable. On `STANDARD_IA`, `ONEZONE_IA`, or `GLACIER_IR`, nothing below 128 KB can reduce your
bill, so a `min_size` at or above `128KB` skips the objects where compression is pure cost.

**`algorithm` is safe to change.** A mount decodes every algorithm ObjectFS can write — `zstd`, `lz4`,
and `gzip` — whichever one it is configured to write, because the codec is chosen from the object's
stored `Content-Encoding` rather than from the configuration. Setting `enabled: false` is safe for the
same reason: it stops new objects being compressed and still decodes the ones already in the bucket.
Verified against the in-process endpoint as a full matrix — every writable algorithm read back by every
algorithm and by a disabled mount — in `TestEveryConfiguredAlgorithmReadsEveryStoredEncoding`.

This is new in v0.11.0. Through v0.10.x a mount decoded only its configured algorithm, so switching
from zstd to gzip made every existing zstd object unreadable, and so did turning compression off. That
read failed closed with a `DATA_CORRUPTION` error rather than returning the raw frame, which was the
correct half of it, but it meant the algorithm was effectively a property of the bucket rather than a
knob to tune. Fixed in [#230](https://github.com/scttfrdmn/objectfs/issues/230); every codec was
already compiled into the same binary, and only the single-codec `Compressor` stood between them and
the read path.

What remains a one-way door is the **format**, not the algorithm: a compressed object is still only
readable by something that understands its `Content-Encoding`, which is cost #1 at the top of this
page.

Compression applies to whole objects on the write path. It does not compress the read cache, and it does
not change what the persistent cache stores on local disk.

---

## How the numbers on this page were measured

Every figure above is either an AWS-published value, linked to the AWS page that publishes it, or a
measurement with its parameters stated. Nothing here is estimated.

- **Bucket** `objectfs-test-scttfrdmn`, region `us-west-2`. **Date** 2026-08-02.
- **Client** a macOS laptop outside the region, so the wall-clock figures include internet round trip
  and local bandwidth. This is why byte counts are presented as the result and latency only as an aside:
  bytes are a property of the design, latency is a property of the day.
- **Payloads** for the amplification table alternate 4 KiB of low-entropy generated text with 4 KiB of
  `/dev/urandom`, giving a 44.8% zstd level 3 ratio at every size. The CSV in the exit-integrity table is
  277,793 bytes of `row<N>,<N>` lines.
- **Objects were written by `aws s3api put-object`**, not by ObjectFS, with the body, `Content-Encoding`,
  and `objectfs-original-size` metadata ObjectFS would have written. That keeps the reader side —
  which is what these tables measure — independent of the writer.
- **Request behavior** (which GETs are issued, ranged or whole, and how many bytes each moves) is pinned
  in CI without touching AWS, against the in-process
  [substrate](https://github.com/scttfrdmn/substrate) endpoint: see
  `TestSmallReadOfLargeObjectDoesNotFetchTheWholeThing` and
  `TestSmallReadOfCompressedObjectStaysCorrect` in `internal/storage/s3/read_amplification_test.go`,
  and `internal/storage/s3/parallel_read_encoding_test.go` for cost 3's request counts.

---

## Related

- [Read-ahead & Predictive Caching](read-ahead.md) — what is served without an S3 request at all
- [Multipart Uploads](multipart-uploads.md) — the other decision that depends on object size
- [#184](https://github.com/scttfrdmn/objectfs/issues/184) — skip data that is already compressed
- [#185](https://github.com/scttfrdmn/objectfs/issues/185) — seekable framing, which removes costs 2 and 3
