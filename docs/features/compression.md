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

### 3. Enabling compression turns off parallel range reads for the whole bucket

Parallel reads fan a large read out into concurrent range GETs above
`performance.parallel_read.threshold`. The gate that selects that path is closed whenever a compression
codec is configured — not whenever the object being read is compressed. So on a mount with compression
enabled, a large read of an object that was never compressed loses the fan-out too.

For compressed objects this is correct: there is nothing to fan out across, since the whole frame is
needed. For the rest of the bucket it is a straightforward loss, and it is the residue of audit finding
C4 — the whole-object-versus-ranged decision was moved onto the object, and this gate one line above it
was not. Tracked as [#228](https://github.com/scttfrdmn/objectfs/issues/228).

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
`TestTierSizeThresholdsMatchWhatAWSPublishes`. That the table is a *write gate* rather than a billing
hint remains its own defect ([#154](https://github.com/scttfrdmn/objectfs/issues/154)). The AWS pages
are the authority, not this page and not that table.

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
- **It adds a failure mode.** An object whose stored encoding this mount cannot decode fails closed with
  an integrity error. That is the correct behavior — v0.10.0 returned the raw compressed frame with exit
  status 0, which is audit finding C2 — but it is a way for a read to fail that an uncompressed object
  does not have. The way to reach it is to change `algorithm` after data has been written — see below,
  a mount decodes only its configured algorithm.

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

**Do not change `algorithm` once a bucket holds compressed objects.** A mount decodes only the one
algorithm it is configured for, so switching from zstd to gzip makes every existing zstd object
unreadable — the read fails with a `DATA_CORRUPTION` integrity error naming the encoding it could not
handle. Verified against the in-process endpoint: an object written with `algorithm: zstd` reads
correctly under `zstd` and fails under each of `gzip`, `lz4`, and `none`.

Failing is the right behavior — v0.10.0 returned the raw compressed frame with exit status 0 — but it
means the algorithm is effectively a property of the bucket, not a knob to tune. If you must change it,
rewrite the objects through the new configuration. There is no in-place migration, and ObjectFS does not
detect the situation in advance. Tracked as
[#230](https://github.com/scttfrdmn/objectfs/issues/230): every codec is compiled in, so dispatching on
each object's stored `Content-Encoding` would make this work, and only the single-codec `Compressor`
prevents it.

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
  `TestSmallReadOfCompressedObjectStaysCorrect` in `internal/storage/s3/read_amplification_test.go`.

---

## Related

- [Read-ahead & Predictive Caching](read-ahead.md) — what is served without an S3 request at all
- [Multipart Uploads](multipart-uploads.md) — the other decision that depends on object size
- [#184](https://github.com/scttfrdmn/objectfs/issues/184) — skip data that is already compressed
- [#185](https://github.com/scttfrdmn/objectfs/issues/185) — seekable framing, which removes cost 2
- [#228](https://github.com/scttfrdmn/objectfs/issues/228) — the parallel-read gate in cost 3
