package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsname"
)

// TestMountBlockReachesTheConfigurationFromAFile is the load-path test, and it is the one that
// matters most for #134.
//
// The block exists for an invocation that cannot pass a bucket on the command line: a systemd template
// unit knows only `%i`, so `objectfs mount --config /etc/objectfs/research-data.yaml` has to find the
// URI and the mount point in the file. The loader decodes strictly, so a `mount:` key the schema does
// not define is a hard error — which means "the struct field exists" and "a config file can set it" are
// two different claims, and only this one is about the file. It is also the exact defect being fixed:
// the keys an operator writes and the keys the program reads had drifted apart before.
func TestMountBlockReachesTheConfigurationFromAFile(t *testing.T) {
	t.Parallel()

	const doc = `mount:
  uri: s3://research-data/lab/2026
  mount_point: /mnt/objectfs/research-data
storage:
  s3:
    region: us-west-2
`

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := NewDefault()
	if err := cfg.LoadFromFile(path); err != nil {
		t.Fatalf("a config file naming what to mount was rejected, so a systemd template unit has "+
			"nowhere to put a per-instance bucket: %v", err)
	}

	if cfg.Mount.URI != "s3://research-data/lab/2026" {
		t.Errorf("mount.uri = %q, want the document's value; this is the bucket the instance mounts",
			cfg.Mount.URI)
	}
	if cfg.Mount.MountPoint != "/mnt/objectfs/research-data" {
		t.Errorf("mount.mount_point = %q, want the document's value; a wrong one is not a mount that "+
			"fails, it is a mount that succeeds somewhere the operator is not looking",
			cfg.Mount.MountPoint)
	}
}

// TestMountBlockIsOptional asserts that a file with no mount block still loads and validates.
//
// The interactive form — `objectfs mount s3://bucket /mnt` — supplies both on the command line, and
// every config file shipped before v0.11.0 has no mount block at all. If an absent block were an
// error, this change would break every existing deployment.
func TestMountBlockIsOptional(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()

	if cfg.Mount.URI != "" || cfg.Mount.MountPoint != "" {
		t.Errorf("NewDefault() mount block = %+v, want both keys empty: a default bucket or a default "+
			"mount point is a mount of something nobody asked for", cfg.Mount)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("the default configuration, whose mount block is empty, does not validate: %v", err)
	}
}

func TestValidateRejectsAnUnusableMountBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mount    MountConfig
		mustName string // a substring the message has to contain, or "" to accept
		why      string
	}{
		{
			name:  "both unset",
			mount: MountConfig{},
			why:   "the interactive form, and every config file written before v0.11.0",
		},
		{
			name:  "a URI alone",
			mount: MountConfig{URI: "s3://research-data"},
			why:   "the mount point may still come from the command line",
		},
		{
			name:  "a mount point alone",
			mount: MountConfig{MountPoint: "/mnt/objectfs/research-data"},
			why:   "and likewise the URI",
		},
		{
			name:  "a bucket with a prefix",
			mount: MountConfig{URI: "s3://research-data/lab/2026", MountPoint: "/mnt/x"},
		},
		{
			name:  "an uppercase bucket",
			mount: MountConfig{URI: "s3://Research-Data", MountPoint: "/mnt/x"},
			why: "accepted, and deliberately: uppercase names were creatable in us-east-1 until 2018 and " +
				"real S3 still serves them — HeadBucket on one returns 404, not 400. This case asserted " +
				"a rejection until the validator was narrowed from S3's CreateBucket rules to " +
				"mountability, which is a different question. See awsname.ValidateBucketName",
		},

		{
			name:     "a bucket name with no scheme",
			mount:    MountConfig{URI: "research-data"},
			mustName: "mount.uri",
			why: "the most likely thing in this key, because the operator is naming a bucket and the " +
				"YAML key is called uri",
		},
		{
			name:     "a bucket name S3 itself calls malformed",
			mount:    MountConfig{URI: "s3://ab"},
			mustName: "mount.uri",
			why: "two characters; HeadBucket returns 400 where a well-formed unknown name returns 404. " +
				"This replaces an uppercase case that the narrowed validator now accepts, so the block " +
				"still covers a bucket-name rejection reaching mount.uri",
		},
		{
			name:     "a scheme this build cannot mount",
			mount:    MountConfig{URI: "gs://research-data"},
			mustName: "mount.uri",
			why: "refused at load rather than at mount, which is the whole reason the validator moved " +
				"to internal/awsname — internal/config cannot import internal/adapter",
		},
		{
			name:     "credentials in the URI",
			mount:    MountConfig{URI: "s3://AKIA...:secret@research-data"},
			mustName: "mount.uri",
			why: "and this one especially at load: accepted, it would have mounted the bucket while " +
				"ignoring the credentials, leaving a secret key in a file the journal quotes",
		},

		{
			name:     "a relative mount point",
			mount:    MountConfig{MountPoint: "mnt/objectfs"},
			mustName: "mount.mount_point",
			why: "a config file has no reliable working directory; a systemd unit's is whatever " +
				"WorkingDirectory happens to be, which is / unless set",
		},
		{
			name:     "a mount point containing ..",
			mount:    MountConfig{MountPoint: "/mnt/objectfs/../objectfs2"},
			mustName: "mount.mount_point",
			why: "the cleaned form is what gets mounted over, and the operator would be reading the " +
				"other one — so the message has to show both",
		},
		{
			name:     "the suggestion is the cleaned path",
			mount:    MountConfig{MountPoint: "/mnt//objectfs/"},
			mustName: "/mnt/objectfs",
			why:      "the message contains the line to write, not a description of it",
		},
		{
			name:     "the root filesystem",
			mount:    MountConfig{MountPoint: "/"},
			mustName: "root filesystem",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			cfg.Mount = tt.mount
			err := cfg.Validate()

			if tt.mustName == "" {
				if err != nil {
					t.Fatalf("Validate() rejected %+v: %v. %s", tt.mount, err, tt.why)
				}

				return
			}

			if err == nil {
				t.Fatalf("Validate() accepted mount %+v; it must be rejected at load, because the "+
					"alternative is a failure after the mount point exists, from whichever S3 call ran "+
					"first, naming neither the file nor the key. %s", tt.mount, tt.why)
			}
			if !strings.Contains(err.Error(), tt.mustName) {
				t.Errorf("Validate() = %q, which does not contain %q, so it does not say what to edit. %s",
					err, tt.mustName, tt.why)
			}
		})
	}
}

// TestMountURIValidationIsAwsnamesAndNotASecondOpinion is the delegation property, stated the same way
// the adapter's is.
//
// Both entry points must agree about what a URI is: a config file that loads must name a URI the mount
// will accept, and vice versa. A table of URIs here would restate awsname's and would pass just as well
// if this package grew its own opinion — which is precisely the shape that gave the repository four size
// parsers (#159) and two region checks before that.
func TestMountURIValidationIsAwsnamesAndNotASecondOpinion(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{
		"s3://research-data", "s3://research-data/lab", "s3://research-data/", "research-data",
		"gs://research-data", "s3://", "s3:///research-data", "s3://Research-Data", "s3://research_data",
		"s3://ab", "s3://b?versionId=x", "s3://u:p@b", "s3://b:9000", "://invalid",
	} {
		t.Run(uri, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			cfg.Mount.URI = uri

			// NewDefault validates cleanly — TestMountBlockIsOptional asserts exactly that — so any
			// error here is this key's.
			gotErr := cfg.Validate() != nil
			wantErr := awsname.ValidateStorageURI(uri) != nil

			if gotErr != wantErr {
				t.Errorf("Validate() with mount.uri=%q rejected=%v, but awsname rejected=%v: a config "+
					"file would load with a URI the mount then refuses, or the reverse", uri, gotErr, wantErr)
			}
		})
	}
}
