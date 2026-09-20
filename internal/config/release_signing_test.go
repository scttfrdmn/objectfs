package config

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release is signed, keylessly, and the signature is verified by the run that made it.
//
// Before this, nothing in the release path was signed. Every asset carried a `.sha256` sibling,
// and a sibling served from the same release as the asset it describes detects a corrupt download
// and proves nothing at all about origin: whoever can replace an asset can replace its hash file in
// the same motion. The one signing mechanism the repository did have — GPG, for the rpm's embedded
// signature — warns and `exit 0`s when its secret is absent, so it ships unsigned packages by
// default. A signing path whose failure mode is "silently unsigned" is worse than none, because the
// absence is invisible.
//
// `cosign sign-blob --bundle` over an aggregate `checksums.txt` fixes that with no key material and
// no repository secret: the identity in the certificate is this workflow, minted from the run's OIDC
// token. The tests below pin the properties that make it worth having, each of which can be undone
// by an edit that looks like a cleanup:
//
//   - `id-token: write`, without which Fulcio cannot issue a certificate at all.
//   - the signature is *verified* in the same job. An unverified signature is decoration.
//   - the certificate identity has exactly one definition. Two copies drift, and the copy that
//     drifts is the one in the release notes, because nothing executes it.
//   - the signing path reads no secret. That is the whole point, and a secret creeping in would mean
//     it had stopped being keyless without anything looking different.
//   - the per-asset `.sha256` siblings survive. `scripts/install.sh` fetches them by exact name.
//   - cosign-installer is pinned to an exact version, because its floating majors stop at v3.

// signingStepName is the step that produces and verifies the signature. Named as a constant because
// two tests scope themselves to it, and a rename that updated only one of them would silently narrow
// the other to nothing.
const signingStepName = "Checksum every asset and sign the list, keylessly"

// publishJob returns release.yml's `publish` job.
//
// This file used to carry its own `signingWorkflow` struct for the two keys below, with a comment
// saying it was local so the file could merge in either order relative to #500's `workflowFile` and
// that the two should converge once both had landed. Both landed; this is that convergence (#504).
func publishJob(t *testing.T) workflowJobDef {
	t.Helper()

	wf := readWorkflow(t, "release.yml")

	job, ok := wf.Jobs["publish"]
	if !ok {
		t.Fatalf(".github/workflows/release.yml has no `publish` job. It has: %v", wf.Jobs)
	}

	return job
}

// releaseWorkflowSource returns release.yml verbatim.
func releaseWorkflowSource(t *testing.T) string {
	t.Helper()

	return workflowSource(t, "release.yml")
}

// TestPublishCanMintAnOIDCToken asserts the permission keyless signing depends on.
func TestPublishCanMintAnOIDCToken(t *testing.T) {
	t.Parallel()

	publish := publishJob(t)

	if got := publish.Permissions["id-token"]; got != "write" {
		t.Errorf("release.yml's publish job has id-token=%q, want \"write\".\n"+
			"Keyless signing exchanges this run's OIDC token for a short-lived Fulcio certificate; "+
			"without the permission there is no token to exchange and `cosign sign-blob` fails at the "+
			"only moment this job ever runs — on a tag, after the image has already been pushed.", got)
	}

	// Still able to create the release. Adding a permission block is a common way to lose one.
	if got := publish.Permissions["contents"]; got != "write" {
		t.Errorf("release.yml's publish job has contents=%q, want \"write\" — `gh release create` "+
			"cannot publish without it", got)
	}
}

// TestTheSignatureIsVerifiedWhereItIsMade asserts the run that signs also verifies.
func TestTheSignatureIsVerifiedWhereItIsMade(t *testing.T) {
	t.Parallel()

	// Scoped to the signing step, not the whole file, and that is the difference between a test and
	// the appearance of one. The first version searched all of release.yml, and the mutation that
	// replaced the executed `cosign verify-blob` with `true` still passed — because the release notes
	// *print* the same command as documentation for users, a dozen lines further down. So the check
	// was matching prose while the verification was gone. Found by mutation, not by reading.
	step, ok := cutStep(releaseWorkflowSource(t), signingStepName)
	if !ok {
		t.Fatalf("release.yml has no step named %q. If it was renamed, rename it here too; if it was "+
			"removed, the release is unsigned again.", signingStepName)
	}

	source := withoutComments(step)

	for _, want := range []string{"cosign sign-blob", "cosign verify-blob", "--bundle"} {
		if !strings.Contains(source, want) {
			t.Errorf("release.yml's signing step no longer runs %q.\n"+
				"Signing without verifying ships a signature nobody has ever checked, and the thing "+
				"that breaks silently is the certificate identity: it comes from the OIDC token, so "+
				"moving or renaming this workflow changes it. If that string is wrong the release "+
				"notes' instructions do not work, and the first person to find out is a user.", want)
		}
	}

	// `--bundle`, not `--output-signature` plus `--output-certificate`. The bundle carries the Rekor
	// transparency-log inclusion proof as well, which is what makes verification a single step.
	if strings.Contains(source, "--output-signature") {
		t.Error("release.yml uses `cosign --output-signature`. Prefer `--bundle`: it carries the " +
			"signature, the certificate and the Rekor inclusion proof in one asset, so a user runs " +
			"one command and the log proof is not left out.")
	}
}

// cosignIdentityFlag matches a literal `--certificate-identity` argument that is not a variable
// reference, i.e. a second hard-coded copy of the identity.
var cosignIdentityFlag = regexp.MustCompile(`--certificate-identity\s+'?"?\$?\{?[^\s'"]*`)

// TestTheCertificateIdentityHasOneDefinition asserts the identity string is not duplicated.
//
// It is used twice — by `cosign verify-blob`, and by the instructions printed into the release notes
// — and only one of those is executed. So a literal in each is a drift that CI cannot see: the notes
// would keep telling users to pass a string that no longer matches the certificate.
func TestTheCertificateIdentityHasOneDefinition(t *testing.T) {
	t.Parallel()

	source := withoutComments(releaseWorkflowSource(t))

	const envKey = "COSIGN_IDENTITY"

	if !strings.Contains(source, envKey+":") {
		t.Fatalf("release.yml's publish job does not define %s. The certificate identity is needed "+
			"both by `cosign verify-blob` and by the release notes; define it once in the job's `env:` "+
			"so the command that is executed and the command that is documented cannot disagree.", envKey)
	}

	uses := strings.Count(source, "$"+envKey) + strings.Count(source, "${"+envKey+"}")
	if uses < 2 {
		t.Errorf("release.yml references $%s %d time(s); expected at least 2 (the verify step and "+
			"the release notes). If one of them went back to a literal, that is the drift this test "+
			"exists to catch.", envKey, uses)
	}

	for _, match := range cosignIdentityFlag.FindAllString(source, -1) {
		if strings.Contains(match, "$") {
			continue
		}

		t.Errorf("release.yml passes a hard-coded --certificate-identity: %q.\n"+
			"Use $%s instead. A second literal copy of that string drifts from the first, and the "+
			"copy that drifts is whichever one is not executed.", match, envKey)
	}
}

// TestTheSigningPathIsKeyless asserts no secret reaches the signing step.
//
// This is the property the user asked for by name: no GPG key, nothing to store, nothing to rotate,
// nothing to leak. It is also load-bearing for trustworthiness — a secret-based signer degrades to
// unsigned when the secret is missing, and this one cannot, because there is nothing to be missing.
func TestTheSigningPathIsKeyless(t *testing.T) {
	t.Parallel()

	source := releaseWorkflowSource(t)

	step, ok := cutStep(source, signingStepName)
	if !ok {
		t.Fatalf("release.yml has no step named %q. If it was renamed, rename it here too; if it was "+
			"removed, the release is unsigned again.", signingStepName)
	}

	if strings.Contains(step, "secrets.") {
		t.Error("the keyless signing step in release.yml references `secrets.`.\n" +
			"Keyless signing needs no secret, and introducing one gives the mechanism a failure mode " +
			"it does not currently have: absent secret means unsigned, which is what the deleted GPG " +
			"path did (it warned and exited 0, so a release shipped unsigned and green). If a secret is " +
			"genuinely required, this is no longer the keyless design and the release notes' claim " +
			"about it is wrong.")
	}

	// cosign should not be handed a key either.
	for _, forbidden := range []string{"--key ", "COSIGN_PRIVATE_KEY", "cosign generate-key-pair"} {
		if strings.Contains(step, forbidden) {
			t.Errorf("the signing step in release.yml uses %q, which makes it key-based rather than "+
				"keyless", forbidden)
		}
	}
}

// TestEveryAssetKeepsItsChecksumSibling asserts the per-asset `.sha256` contract survives.
//
// `scripts/install.sh` fetches `<asset>.sha256` by exact name for the platform it detected, and the
// installer is verified end to end against real published releases. An aggregate `checksums.txt` is
// an addition, not a replacement: dropping the siblings would break every installer invocation on
// the next release, and the installer's own tests would not catch it because they read the script
// rather than the release.
//
// Where the siblings come from has changed and the contract has not. release.yml used to write them
// itself, twice — `sha256sum "$BINARY_NAME.tar.gz" > "$BINARY_NAME.tar.gz.sha256"` in the build matrix
// and `sha256sum "$pkg" > "$pkg.sha256"` in the packaging job — and this test named both literals.
// goreleaser writes them now, and the setting that decides is one word:
//
//	checksum:
//	  split: true
//
// `split: false`, which is goreleaser's default, produces a single `checksums.txt` and *no* siblings.
// So this is not a preference: the default breaks the documented installer on every platform at once,
// and it breaks it in a way that looks like a working release — the tarball is published, the
// checksum document is published, and install.sh dies on a 404 for a file that used to exist.
func TestEveryAssetKeepsItsChecksumSibling(t *testing.T) {
	t.Parallel()

	// The siblings, from the config that writes them. Read as text rather than through readGoreleaser's
	// struct because what matters is the two lines together: `split` under `checksum`, not a `split:`
	// anywhere in the file.
	packaging := withoutComments(readFile(t, filepath.Join(repoRoot(t), packagingFile)))

	checksum, _, found := strings.Cut(packaging, "\nnfpms:")
	if _, after, ok := strings.Cut(checksum, "\nchecksum:"); ok && found {
		if !strings.Contains(after, "split: true") {
			t.Errorf("%s's `checksum:` section does not set `split: true`. goreleaser's default writes "+
				"one combined checksums.txt and no `<asset>.sha256` siblings, and scripts/install.sh "+
				"fetches a sibling by exact name and treats its absence as fatal — so the next release "+
				"would publish every tarball and break the documented one-liner on every platform.",
				packagingFile)
		}
	} else {
		t.Errorf("%s has no `checksum:` section before its `nfpms:` section. Without one goreleaser "+
			"still writes checksums, using its own defaults, which do not include the per-asset "+
			"siblings scripts/install.sh requires.", packagingFile)
	}

	// And the publish job has to keep requiring them. This is the half that catches a sibling that
	// silently stops being produced for one asset kind rather than for all of them: the loop below
	// walks every asset it is about to attach and fails on the first one with no sibling.
	source := withoutComments(releaseWorkflowSource(t))

	if !strings.Contains(source, `if [ ! -f "$asset.sha256" ]`) {
		t.Error("release.yml's publish job no longer checks that each asset has its `.sha256` sibling " +
			"before attaching it. That check is what makes a goreleaser default change visible on the " +
			"release that introduces it rather than on the first install after it.")
	}

	if !strings.Contains(source, "checksums.txt") {
		t.Error("release.yml no longer produces checksums.txt, which is the document the keyless " +
			"signature covers. Without it the signature has nothing to attest to. goreleaser does not " +
			"write it under `split: true` — the publish job builds it from the assets and cross-checks " +
			"it against the siblings, so both hash sources are proven to agree before either is served.")
	}
}

// TestCosignInstallerIsPinnedToAnExactVersion asserts the action reference resolves.
//
// Not a style rule. sigstore/cosign-installer publishes moving `v2` and `v3` refs and stopped there:
// `repos/sigstore/cosign-installer/git/ref/tags/v4` is a 404 while `v4.1.2` resolves. So `@v4` is
// not "the latest v4", it is a reference that cannot be resolved — and it would fail on a tag, after
// the artifacts exist and the image has been pushed. Measured against the API, not assumed.
func TestCosignInstallerIsPinnedToAnExactVersion(t *testing.T) {
	t.Parallel()

	source := withoutComments(releaseWorkflowSource(t))

	const action = "sigstore/cosign-installer@"

	_, after, found := strings.Cut(source, action)
	if !found {
		t.Fatal("release.yml does not use sigstore/cosign-installer, so nothing installs cosign and " +
			"the signing step cannot run")
	}

	fields := strings.Fields(after)
	if len(fields) == 0 {
		t.Fatal("release.yml uses sigstore/cosign-installer with no ref at all")
	}

	ref := fields[0]

	// vN alone is the failure mode; vN.N.N or a 40-char sha is fine.
	if regexp.MustCompile(`^v\d+$`).MatchString(ref) {
		t.Errorf("release.yml pins sigstore/cosign-installer@%s, a floating major.\n"+
			"Its floating majors stop at v3 — `git/ref/tags/v4` is a 404 — so a bare major above that "+
			"does not resolve, and it would fail on a tag with the image already pushed. Pin an exact "+
			"version or a commit sha; Dependabot updates either.", ref)
	}
}

// cutStep moved to workflow_test.go alongside the one workflow parser (#504), with the measurement
// that justifies it: `cosign verify-blob` appears twice in release.yml and only one of them runs, so
// a mutation deleting the executed one passed an assertion that searched the whole file.
