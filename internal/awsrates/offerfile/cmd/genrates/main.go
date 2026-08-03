// Command genrates regenerates internal/awsrates/rates_generated.go from the AWS price list.
//
// It is invoked by the //go:generate directive in internal/awsrates, and it is the only thing in this
// repository that talks to AWS's pricing endpoints. Nothing on the mount path does: the generated
// table is compiled in, so pricing a tier needs no credentials, no network, and cannot fail a
// filesystem operation. That is the constraint #161 sets and the reason the live-fetch design in #227
// is a separate decision rather than a natural extension of this one.
//
// Usage:
//
//	go run ./internal/awsrates/offerfile/cmd/genrates -o internal/awsrates/rates_generated.go
//	go run ./internal/awsrates/offerfile/cmd/genrates -dry-run
//
// A region that publishes no S3 rates is skipped with a line on stderr rather than failing the run.
// Three of the entries in AWS's S3 region index are local zones whose offer files contain no S3
// storage product at all; failing on them would mean this command could never succeed, and silently
// dropping them would mean a real region disappearing from the table without anyone noticing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/scttfrdmn/objectfs/internal/awsrates/offerfile"
)

func main() {
	out := flag.String("o", "", "write the generated Go file here (required unless -dry-run)")
	dryRun := flag.Bool("dry-run", false, "fetch and extract, print a summary, write nothing")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall deadline for the whole run")
	flag.Parse()

	if err := run(offerfile.NewFetcher(), *out, *dryRun, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "genrates: %v\n", err)
		os.Exit(1)
	}
}

// run takes its fetcher rather than constructing one, so the decisions below — which regions to skip,
// when to refuse to write, what -dry-run does — are testable against a local server instead of only
// against AWS. They are the parts of this command that can be wrong in a way that produces a file: a
// bad skip rule silently drops a region from every price ObjectFS quotes, and that is not visible in
// the output of a successful run.
func run(f *offerfile.Fetcher, out string, dryRun bool, timeout time.Duration) error {
	if out == "" && !dryRun {
		return errors.New("-o is required unless -dry-run is set")
	}

	// Interrupt as well as the deadline: this makes ~80 HTTP requests, and a run someone cancels
	// should stop rather than finish fetching.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	codes, err := f.Regions(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "genrates: %d regions in the S3 offer index\n", len(codes))

	regions := make([]offerfile.Region, 0, len(codes))

	for _, code := range codes {
		r, err := f.Region(ctx, code)

		switch {
		case errors.Is(err, offerfile.ErrNoS3Rates):
			fmt.Fprintf(os.Stderr, "genrates: %s publishes no S3 rates (local zone), skipping\n", code)

			continue
		case err != nil:
			return fmt.Errorf("%s: %w", code, err)
		}

		regions = append(regions, r)

		std := r.Rates["STANDARD"]
		fmt.Fprintf(os.Stderr, "genrates: %-20s prefix=%-8q standard=%v egress=%v\n",
			code, r.Prefix, std.StoragePerGBMonth, std.EgressPerGB)
	}

	if len(regions) == 0 {
		return errors.New("no region yielded rates; refusing to write an empty table")
	}

	src, err := offerfile.Emit(regions)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "genrates: %d regions, %d bytes of Go, nothing written\n",
			len(regions), len(src))

		return nil
	}

	// 0o644, and written whole rather than in place: a partial write here leaves the package
	// unbuildable, and this file is the only source of every price ObjectFS reports.
	//
	// Suppressed twice because there are two gosec runs and they read different directives. golangci-lint
	// honors //nolint:gosec; the standalone gosec in .github/workflows/security.yml, whose SARIF feeds
	// GitHub code scanning, honors only #nosec. A finding suppressed in one and not the other passes the
	// lint job and fails the security check, which is how this line was found.
	//
	//nolint:gosec // G306 wants 0o600. The output is Go source that gets committed and compiled into
	// every build; it holds published AWS list prices and nothing secret, and a file mode narrower
	// than the rest of the tree's .go files would make it the one source file a reader cannot read.
	if err := os.WriteFile(out, src, 0o644); err != nil { // #nosec G306 -- committed Go source, no secrets
		return fmt.Errorf("write %s: %w", out, err)
	}

	fmt.Fprintf(os.Stderr, "genrates: wrote %s (%d regions, %d bytes)\n", out, len(regions), len(src))

	return nil
}
