package awsname

import (
	"fmt"
	"net"
	"strings"
)

// Bucket-name limits, from the S3 general-purpose bucket naming rules.
const (
	// bucketNameMin is 3 characters. Verified against real S3 rather than read from the documentation:
	// HeadBucket on "b" and on "ab" returns 400 Bad Request, where a well-formed name for a bucket that
	// does not exist returns 404.
	bucketNameMin = 3

	// bucketNameMax is 63 characters, which is the DNS label limit rather than an S3 one — a bucket name
	// has to be usable as the leftmost label of `<bucket>.s3.<region>.amazonaws.com`.
	bucketNameMax = 63
)

// bucketNameReservedSuffixes are suffixes that name a bucket type or access mechanism ObjectFS cannot
// mount, as opposed to one it has not tried.
//
// `--x-s3` is a directory bucket (Express One Zone), `--table-s3` an S3 Tables bucket, `--ol-s3` an
// Object Lambda access point alias, `-s3alias` an access point alias, `.mrap` a multi-region access
// point. Each addresses something with a different API surface: directory buckets have no object
// annotations and a different listing model, access points are not buckets at all. Refused here so the
// message says that, rather than letting the mount come apart later on whichever call first depends on a
// feature the endpoint does not have.
var bucketNameReservedSuffixes = []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"}

// bucketNameForbiddenBytes are the characters that cannot appear in a name this code will put into a URL.
//
// The name is interpolated into a hostname for virtual-hosted addressing and into a path for path-style,
// so each of these would not produce a rejected request — it would produce a request somewhere else, or a
// malformed one, at a layer with no idea the name came from an operator's config file. A slash ends the
// authority; a colon starts a port; an at-sign starts userinfo; a question mark or hash starts a query or
// fragment; whitespace and control bytes break the request line.
const bucketNameForbiddenBytes = "/\\:@?#[]%& \t\n\r\v\f\x00"

// ValidateBucketName reports whether name is one ObjectFS can mount.
//
// Checked at config load rather than at the first API call, because the failure it prevents is not a
// rejected request. A mount is a long-lived process started by an init system: an operator who typos a
// bucket name into a per-instance config file otherwise gets `systemctl start` failing at some point
// after the FUSE mount already exists, with a message from whichever S3 call happened to be first.
//
// It is deliberately narrower than S3's *CreateBucket* rules, and the difference is the point. Those
// rules govern what can be created today; this governs what can be mounted, and buckets predating them
// exist. Verified against real S3 in us-west-2: HeadBucket on `MyBucket` returns 404 and on `my_bucket`
// returns 403 — well-formed names for buckets this account cannot see — while `b` and `ab` return 400.
// Uppercase letters and underscores were creatable in us-east-1 until 2018, so a validator applying the
// creation rules would refuse to mount a bucket that exists and that S3 will serve. v0.10.3 accepted any
// non-empty host, so that would also be a silent regression for whoever owns one.
//
// What is refused is therefore only what cannot work: a name S3 rejects outright on length, a name that
// cannot be placed in a URL, an IP-address-shaped name, and a name identifying a bucket type with a
// different API. A legacy name is accepted; if the SDK cannot reach it with virtual-hosted addressing,
// storage.s3.force_path_style is the setting for that, and the error S3 returns names the bucket.
func ValidateBucketName(name string) error {
	if name == "" {
		return fmt.Errorf("bucket name is empty")
	}

	if len(name) < bucketNameMin || len(name) > bucketNameMax {
		// "is 1 character", not "is 1 characters". A one-character bucket name is a real invocation —
		// `objectfs mount s3://b /mnt` is what someone types while testing — so this is the message they
		// see, and a message with a grammatical error in it reads as a message nobody has looked at.
		unit := "characters"
		if len(name) == 1 {
			unit = "character"
		}

		return fmt.Errorf("bucket name %q is %d %s; S3 requires %d to %d",
			name, len(name), unit, bucketNameMin, bucketNameMax)
	}

	if i := strings.IndexAny(name, bucketNameForbiddenBytes); i >= 0 {
		return fmt.Errorf("bucket name %q contains %q at position %d, which cannot appear in a bucket "+
			"name — the name goes into a URL, so this would address something other than the bucket "+
			"rather than fail", name, name[i], i)
	}

	for _, r := range name {
		if r > 127 {
			return fmt.Errorf("bucket name %q contains the non-ASCII character %q; S3 bucket names are "+
				"ASCII, and a name that looks right can differ from the one that was typed — a Cyrillic "+
				"с and a Latin c are two characters", name, r)
		}
	}

	// An IP-address-shaped name is refused because the virtual-hosted endpoint would be ambiguous with a
	// literal address. net.ParseIP is the check rather than counting dots, so "1.2.3.04" and "999.1.1.1"
	// — neither a valid address — stay legal, as S3 has them.
	if net.ParseIP(name) != nil {
		return fmt.Errorf("bucket name %q is formatted as an IP address, which S3 rejects", name)
	}

	for _, suffix := range bucketNameReservedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return fmt.Errorf("bucket name %q ends with %q, which identifies a bucket type or access "+
				"mechanism ObjectFS does not mount: these have a different API surface — a directory "+
				"bucket has no object annotations and a different listing model, and an access point "+
				"alias is not a bucket", name, suffix)
		}
	}

	return nil
}
