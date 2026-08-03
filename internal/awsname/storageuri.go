package awsname

import (
	"fmt"
	"net/url"
	"strings"
)

// StorageURI is a parsed object storage URI: the bucket to mount and an optional prefix within it.
type StorageURI struct {
	// Bucket is the S3 bucket name, taken from the URI's host.
	Bucket string

	// Prefix is the path within the bucket, with leading and trailing slashes removed, or empty for
	// the whole bucket. Normalized so that "s3://b", "s3://b/" and "s3://b//" are one mount and not
	// three, since a prefix is concatenated with object keys and a stray slash produces a key with an
	// empty path component that no other S3 tool would write.
	Prefix string
}

// ParseStorageURI parses and validates an object storage URI — "s3://bucket" or "s3://bucket/prefix".
//
// It lives in this package for the reason the package exists: as of v0.11.0 a URI can arrive either as
// a command-line argument, which internal/adapter validates, or as `mount.uri` in a configuration
// file, which internal/config validates. config cannot import adapter, so before this the two would
// each have kept their own opinion of what a URI is — which is the four-parsers shape that #159 had
// just finished removing from the size settings, and audit finding C1's shape before that.
//
// Only s3:// is accepted. gs://, azure:// and minio:// appear in cmd/objectfs/doc.go under "Future
// Support" and nothing in this build can mount any of them, so they are rejected by name rather than
// producing a bucket called "container-name" and a failure several layers down.
func ParseStorageURI(uri string) (StorageURI, error) {
	if strings.TrimSpace(uri) == "" {
		return StorageURI{}, fmt.Errorf("no storage URI given; write one as s3://bucket or " +
			"s3://bucket/prefix")
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		return StorageURI{}, fmt.Errorf("storage URI %q cannot be parsed: %w", uri, err)
	}

	// A bare bucket name is the mistake worth naming, because it is what a person writes when they
	// have not seen the URI form and it parses cleanly as a relative path — empty scheme, empty host,
	// the name in Path. Without this it would be reported as an unsupported scheme of "".
	if parsed.Scheme == "" {
		return StorageURI{}, fmt.Errorf("storage URI %q has no scheme; write it as s3://%s", uri, uri)
	}

	if parsed.Scheme != "s3" {
		return StorageURI{}, fmt.Errorf("storage URI %q uses scheme %q; only s3:// is supported in "+
			"this build", uri, parsed.Scheme)
	}

	// url.Parse puts the bucket in Host for "s3://bucket". A URI written with three slashes —
	// "s3:///bucket" — leaves Host empty, and that is a missing bucket rather than a bucket named
	// "bucket", because the same string means an empty authority to every other URI consumer.
	if parsed.Host == "" {
		return StorageURI{}, fmt.Errorf("storage URI %q names no bucket; write it as s3://bucket", uri)
	}

	// The four components url.Parse accepts and this URI form has no meaning for. Each is refused by
	// name because the alternative is silence: url.Parse puts them in fields nothing downstream reads,
	// so `s3://bucket?versionId=x` would mount `bucket` and the operator would have no way to learn
	// that the half of the line they cared about was discarded.
	if parsed.User != nil {
		return StorageURI{}, fmt.Errorf("storage URI %q carries credentials in the URI; ObjectFS "+
			"takes credentials from the AWS credential chain (environment, profile, or instance "+
			"role) and never from a URI, which would put a secret key in a config file, a process "+
			"listing, and a systemd journal", uri)
	}

	if parsed.Port() != "" {
		return StorageURI{}, fmt.Errorf("storage URI %q specifies port %q; an alternative endpoint "+
			"is configured with storage.s3.endpoint, not in the URI", uri, parsed.Port())
	}

	if parsed.RawQuery != "" {
		return StorageURI{}, fmt.Errorf("storage URI %q has a query string (%q); a mount names a "+
			"bucket and an optional prefix and nothing else", uri, parsed.RawQuery)
	}

	if parsed.Fragment != "" {
		return StorageURI{}, fmt.Errorf("storage URI %q has a fragment (%q); a mount names a bucket "+
			"and an optional prefix and nothing else", uri, parsed.Fragment)
	}

	if err := ValidateBucketName(parsed.Host); err != nil {
		return StorageURI{}, fmt.Errorf("storage URI %q: %w", uri, err)
	}

	return StorageURI{
		Bucket: parsed.Host,
		Prefix: strings.Trim(parsed.Path, "/"),
	}, nil
}

// ValidateStorageURI reports whether uri is one this build can mount, discarding the parse.
//
// For a validator that only needs the yes-or-no — [internal/config.Configuration.Validate] checking
// `mount.uri` at load, where nothing is mounted yet and the bucket is not wanted.
func ValidateStorageURI(uri string) error {
	_, err := ParseStorageURI(uri)

	return err
}
