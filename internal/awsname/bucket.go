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

// isBucketNameChar reports whether r can appear in a bucket name: an ASCII letter of either case, a
// digit, '.', '-' or '_'.
//
// An allow-list, and it replaces a deny-list that was the site of two defects. That list,
// bucketNameForbiddenBytes, named the URL-significant characters someone thought of — `/\:@?#[]%&` and
// space — under a comment explaining why each one breaks a URL. It was right about every character it
// named and silent about the rest. The first hole was control bytes: the list enumerated five of
// thirty-three, FuzzValidateBucketName found `"00\x06"` in under a second, and #571 closed it by adding a
// second enumerated category. The second was a backtick: the name 00 followed by one went into a hostname that
// net/url refuses, found in 0.07s the first time CI fuzzed the target at all (#565). Neither list was
// wrong about anything it said; a deny-list is wrong about everything it does not say, and the
// characters it does not say are the ones nobody tries by hand — `"<>^{|}!$'()*+,;=~` and a backtick
// were all still accepted.
//
// So the rule is now what a bucket name can be, and that set is small and known. The widest S3 has ever
// allowed is the legacy us-east-1 rule, which added uppercase letters and underscores to today's
// lowercase, digits, dots and hyphens. ValidateBucketName deliberately accepts legacy names, because
// buckets carrying them exist and S3 serves them — see its comment — so this admits exactly that set.
// No character outside it has ever named a bucket, so refusing it cannot refuse a bucket that exists.
func isBucketNameChar(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return true
	case r == '.' || r == '-' || r == '_':
		return true
	default:
		return false
	}
}

// isControlByte reports whether r is a C0 control character or DEL.
//
// By category, never by enumeration — see bucketNameForbiddenBytes for what enumerating cost. The range
// is ASCII-only because the callers check `r > 127` separately and reject it, so a Unicode control
// character cannot reach a name that gets this far.
func isControlByte(r rune) bool {
	return r < 0x20 || r == 0x7f
}

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

	for i, r := range name {
		if isControlByte(r) {
			return fmt.Errorf("bucket name %q contains the control character %q at position %d, which "+
				"cannot appear in a bucket name — the name goes into a URL, so this would address "+
				"something other than the bucket rather than fail", name, r, i)
		}

		if r > 127 {
			return fmt.Errorf("bucket name %q contains the non-ASCII character %q; S3 bucket names are "+
				"ASCII, and a name that looks right can differ from the one that was typed — a Cyrillic "+
				"с and a Latin c are two characters", name, r)
		}

		// After the two cases above, which keep their own messages because each says something the
		// operator would not otherwise see: a control byte and a lookalike letter are both invisible.
		if !isBucketNameChar(r) {
			return fmt.Errorf("bucket name %q contains %q at position %d, which cannot appear in a bucket "+
				"name: a bucket name is letters, digits, '.', '-' and '_'. The name goes into a URL, so "+
				"this would address something other than the bucket, or fail to parse, rather than be "+
				"refused by S3", name, r, i)
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
