package config

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// The release is signed, keylessly, and the signature is verified by the run that made it.
//
// Before this, nothing in the release path was signed. Nine assets each carried a `.sha256` sibling,
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

// signingWorkflow is the slice of workflow syntax these tests need: the per-job `permissions:` block.
//
// A local parse rather than a shared one, so this file stands alone against `main` and can merge in
// either order relative to the gate change (#500), which adds its own `workflowFile` for a different
// purpose. Once both have landed the two should share one parser; see the follow-up issue.
type signingWorkflow struct {
	Jobs map[string]struct {
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
	} `yaml:"jobs"`
}

// publishJob returns release.yml's `publish` job.
func publishJob(t *testing.T) struct {
	Permissions map[string]string `yaml:"permissions"`
	Env         map[string]string `yaml:"env"`
} {
	t.Helper()

	var wf signingWorkflow
	if err := yaml.Unmarshal([]byte(releaseWorkflowSource(t)), &wf); err != nil {
		t.Fatalf("parsing .github/workflows/release.yml: %v", err)
	}

	job, ok := wf.Jobs["publish"]
	if !ok {
		t.Fatalf(".github/workflows/release.yml has no `publish` job. It has: %v", wf.Jobs)
	}

	return job
}

// releaseWorkflowSource returns release.yml verbatim.
func releaseWorkflowSource(t *testing.T) string {
	t.Helper()

	return readFile(t, filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
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
			"it does not currently have: absent secret means unsigned, which is what the GPG path in " +
			"`package-linux` does (it warns and exits 0). If a secret is genuinely required, this is " +
			"no longer the keyless design and the release notes' claim about it is wrong.")
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
func TestEveryAssetKeepsItsChecksumSibling(t *testing.T) {
	t.Parallel()

	source := withoutComments(releaseWorkflowSource(t))

	// The two places siblings are written: the build matrix, for tarballs, and package-linux, for the
	// deb and the rpm.
	for _, want := range []string{
		`sha256sum "$BINARY_NAME.tar.gz" > "$BINARY_NAME.tar.gz.sha256"`,
		`sha256sum "$pkg" > "$pkg.sha256"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("release.yml no longer contains:\n\t%s\n"+
				"That is how a published asset gets its `.sha256` sibling, and scripts/install.sh "+
				"fetches that exact name. Without it the installer fails on every platform, on the "+
				"next release, and no test that reads install.sh can see it.", want)
		}
	}

	if !strings.Contains(source, "checksums.txt") {
		t.Error("release.yml no longer produces checksums.txt, which is the document the keyless " +
			"signature covers. Without it the signature has nothing to attest to.")
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

// cutStep returns the body of the named workflow step, from its `- name:` line to the next one.
func cutStep(source, name string) (string, bool) {
	idx := strings.Index(source, "- name: "+name)
	if idx < 0 {
		return "", false
	}

	rest := source[idx+len("- name: "+name):]

	if next := strings.Index(rest, "\n    - name: "); next >= 0 {
		rest = rest[:next]
	}

	return rest, true
}
