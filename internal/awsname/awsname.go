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
