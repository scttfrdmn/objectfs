// Package awsname validates the AWS identifiers ObjectFS accepts from configuration.
//
// It exists to be a leaf. The region is read by internal/config and acted on by
// internal/storage/s3, and those two cannot share a validator through either of themselves:
// pkg/types aliases internal/config's types and internal/storage/s3 imports pkg/types, so config
// importing s3 is an import cycle. Without somewhere neutral to put the check, each layer keeps its
// own opinion or — as was the case — neither has one.
//
// That is the same structural gap that produced audit finding C1. There, config could not validate a
// compression algorithm by the only means that cannot go stale (asking the codec package to build
// it) because the dependency ran the wrong way, so config kept a list, the list drifted, and the
// release shipped a default no codec existed for. One authority, in a package both sides can reach.
package awsname

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// maxRegionLength bounds a region at the DNS label limit.
//
// The region is templated into a hostname — s3.<region>.amazonaws.com — so anything longer cannot
// resolve. Bounding it here means the failure is a config error naming the setting rather than a DNS
// lookup failure several layers down.
const maxRegionLength = 63

// regionPattern is the syntax an AWS region has: lowercase alphanumeric groups joined by single
// hyphens. It admits every region AWS has published, including the partitions that are easy to
// forget — us-gov-west-1, cn-north-1, us-iso-east-1, ap-southeast-4.
//
// It is deliberately a syntax check and not a list of known regions. A list would reject every
// region AWS launches after this build, which is a worse failure than accepting a typo: the typo
// surfaces on the first request, and the stale list makes a correct configuration unusable with no
// way for the operator to override it.
var regionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidateRegion reports whether region is a syntactically valid AWS region.
//
// An empty region is valid and means "resolve it from the environment" — the AWS_REGION variable,
// the shared config file, or the instance metadata service. That is the correct default on EC2 and
// for anyone using AWS_PROFILE, and it matches how the static-credential fields already behave: left
// empty, the default chain applies.
//
// Anything non-empty must look like a region, because a malformed one is not caught anywhere else.
// Verified against real S3 in us-west-2:
//
//   - "US-WEST-2" — the AWS SDK builds a client, sends the request, and S3 answers 400. Nothing in
//     the error names the region or suggests the case is the problem.
//   - "us west 2" — fails inside the SDK's endpoint resolver with "resolve auth scheme: resolve
//     endpoint: endpoint rule error", which reads like an SDK bug rather than a typo in a YAML file.
//   - "a/b" — the slash is templated into the endpoint, injecting a path segment into what should be
//     a hostname. The request goes somewhere, and it is not where the operator asked.
//
// Each of those is C1's shape: a value every layer that reads configuration accepts, and only the
// layer that acts on it rejects — by which point the user has asked for a mount and the message
// names no setting. The check is here so the answer is "storage.s3.region is not a valid region"
// before any of it is attempted.
func ValidateRegion(region string) error {
	if region == "" {
		return nil
	}

	if len(region) > maxRegionLength {
		return fmt.Errorf("region %q is %d characters, over the %d-character DNS label limit it "+
			"must fit in to form an S3 endpoint", region, len(region), maxRegionLength)
	}

	if !regionPattern.MatchString(region) {
		return fmt.Errorf("region %q is not a valid AWS region: expected lowercase letters, digits "+
			"and single hyphens, as in \"us-west-2\" or \"eu-central-1\" (an empty region resolves "+
			"from AWS_REGION, the shared config file, or instance metadata)", region)
	}

	return nil
}

// RegionIsResolvable reports whether an empty configured region will resolve to one at mount time.
//
// [ValidateRegion] deliberately accepts "" as "resolve it from the environment", which is correct on
// EC2 and for anyone using AWS_PROFILE. But it is only correct where something is actually there to
// resolve, and where nothing is, the mount fails several layers down inside a HeadBucket health check
// with "failed to resolve service endpoint, endpoint rule error, A region must be set when sending
// requests to S3" — a message that names no key the operator could edit.
//
// FuzzConfigConstructsBackend found this from the input `storage:` alone, and found it on CI rather
// than on a developer's machine. That difference is the finding: a shell with AWS_REGION or
// AWS_PROFILE exported, or a populated ~/.aws/config, resolves the region and hides the defect
// entirely — so it reproduces in a container, a CI runner, and a systemd unit with a clean
// environment, which is to say in production and not in development.
//
// This is separate from ValidateRegion because the two ask different questions. ValidateRegion is
// syntax and is a pure function of its argument. This one asks what the environment will supply, so
// its answer depends on where it runs and it cannot be a table test. Only the *sources* are checked
// here — the two environment variables and AWS_CONFIG_FILE's region key. IMDS is deliberately not
// probed: it would mean a network round trip with a timeout on the config-load path, and on a machine
// that is not EC2 the probe is pure latency before an error message. An EC2 instance whose region
// comes only from metadata therefore still gets the deep failure, which is the one case this does not
// improve; setting storage.s3.region explicitly is the answer, and the error says so.
func RegionIsResolvable(region string) bool {
	if region != "" {
		return true
	}

	for _, env := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		if os.Getenv(env) != "" {
			return true
		}
	}

	// A shared config file that exists is taken at its word rather than parsed. Deciding which profile
	// applies means reproducing the SDK's precedence between AWS_PROFILE, [default], and a
	// credential_source chain — and getting that subtly wrong would refuse a configuration that would
	// have worked, which is worse than the deep error this exists to replace. The SDK remains the
	// authority; this only declines to guess on its behalf.
	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}

		path = filepath.Join(home, ".aws", "config")
	}

	// G703 reads AWS_CONFIG_FILE as a tainted path. It is tainted by the operator of this process,
	// which is the AWS SDK's own contract for that variable — and this only stats it. Nothing here
	// opens, reads, or writes the path, so there is no traversal to perform.
	//
	// G703 exists only in the standalone gosec, not in the version golangci-lint bundles: probed on a
	// two-line program reading os.Getenv, standalone reports G703 and G304 and golangci-lint reports
	// G304 alone. So the //nolint:gosec that used to be here could not have suppressed anything even
	// written correctly — golangci-lint never had the finding, and the standalone run does not read
	// //nolint. #nosec is the directive that reaches the run that reports it.
	info, err := os.Stat(path) // #nosec G703 -- AWS_CONFIG_FILE is operator-supplied by the SDK's own contract; stat only

	return err == nil && !info.IsDir() && info.Size() > 0
}
