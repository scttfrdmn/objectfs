package utils

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePath rejects an empty path, a relative path that escapes the working directory, and — when
// allowAbsolute is false — an absolute path.
//
// It returns an error if, after [filepath.Clean]:
//   - the path is empty
//   - the path is relative and begins with "..", so resolving it leaves the working directory
//   - the path is absolute and allowAbsolute is false
//
// It does *not* check containment within any particular directory, because no base directory is
// supplied; [ValidatePathWithinBase] is that check. What this function offers a caller is a cheap
// refusal of the two shapes an operator most often supplies by mistake.
//
// # What it does and does not catch, given allowAbsolute
//
// Worth stating plainly, because the answer is asymmetric and [#384] was filed partly to settle it.
// Clean removes every ".." it can resolve, and for an *absolute* path it can always resolve them: it
// treats the root as its own parent, so Clean("/../etc/passwd") is "/etc/passwd" and
// Clean("/var/../etc/passwd") is "/etc/passwd". An absolute path therefore never reaches the traversal
// check with a ".." left in it — verified by execution, not read from the documentation.
//
// So with allowAbsolute true, the only thing this function refuses beyond an empty string is a
// *relative* path that climbs out of the working directory: "../x" is rejected while "/etc/passwd" is
// accepted. That is not incoherent — the caller has said absolute paths are the operator's business,
// and a relative path that escapes is more likely a mistake than an intention — but it is narrow, and
// a caller should not read a call to this function as containment.
//
// All three call sites in this repository pass allowAbsolute true and all three take an operator's own
// configuration or log path ([config.Configuration.LoadFromFile], the pricing manager's discount file,
// and the log destination). For them this is a typo check, not a security boundary: an operator who
// can pass --config can pass an absolute path to anything. It is kept because a clear early refusal
// costs nothing, and named honestly here so nobody builds on it as more than that.
//
// Example usage:
//
//	if err := ValidatePath(userProvidedPath, false); err != nil {
//		return fmt.Errorf("invalid path: %w", err)
//	}
//
// [#384]: https://github.com/scttfrdmn/objectfs/issues/384
func ValidatePath(path string, allowAbsolute bool) error {
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}

	// Clean the path to resolve any . or .. elements
	cleanPath := filepath.Clean(path)

	// Reject traversal, which after Clean means a *leading* "..".
	//
	// Deliberately not strings.Contains(cleanPath, ".."), which is what this was and which is the
	// defect #384 reports. Clean has already resolved every resolvable ".." by the time this runs, so
	// a remaining one can only be leading — but Contains also matches an adjacent pair *inside* a
	// component, and those are ordinary filenames. `--config ./objectfs..staging.yaml` failed with
	// "path contains directory traversal", and a log file named `run..1.log` was refused the same way:
	// an error message naming a cause that is not the reason, which is the kind that costs an
	// afternoon. Checked against the separator rather than by prefix on ".." alone, so "..foo" and
	// "...", which are also just names, are not caught by the fix either.
	if cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path contains directory traversal: %s", path)
	}

	// Check if path is absolute when not allowed
	if !allowAbsolute && filepath.IsAbs(cleanPath) {
		return fmt.Errorf("absolute paths not allowed: %s", path)
	}

	return nil
}

// ValidatePathWithinBase validates that a file path is within a specified base directory.
// This is useful for ensuring that user-provided paths don't escape a designated directory.
//
// The function:
//  1. Cleans both the base and target paths
//  2. Joins them together
//  3. Verifies the result stays within the base directory
//
// Example usage:
//
//	if err := ValidatePathWithinBase("/var/cache", userPath); err != nil {
//		return fmt.Errorf("path outside allowed directory: %w", err)
//	}
func ValidatePathWithinBase(base, path string) error {
	if base == "" {
		return fmt.Errorf("base path cannot be empty")
	}
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}

	// Clean both paths
	cleanBase := filepath.Clean(base)
	cleanPath := filepath.Clean(path)

	// If path is absolute, it must be within base
	if filepath.IsAbs(cleanPath) {
		if !strings.HasPrefix(cleanPath, cleanBase+string(filepath.Separator)) &&
			cleanPath != cleanBase {
			return fmt.Errorf("path %s is outside base directory %s", path, base)
		}
		return nil
	}

	// For relative paths, join and validate
	fullPath := filepath.Join(cleanBase, cleanPath)

	// Verify the joined path is still within base
	if !strings.HasPrefix(fullPath, cleanBase+string(filepath.Separator)) &&
		fullPath != cleanBase {
		return fmt.Errorf("path %s escapes base directory %s", path, base)
	}

	return nil
}

// SecureJoin safely joins path elements and ensures the result stays within the base directory.
// Unlike filepath.Join, this function validates that the result doesn't escape the base through
// directory traversal.
//
// # An absolute element is joined, not refused
//
// SecureJoin("/var/cache", "/etc/passwd") returns "/var/cache/etc/passwd" and no error. That is
// [filepath.Join]'s documented behavior — it discards the leading separator rather than restarting at
// the root, which is why the result is still contained — and it is arguably the right answer for a
// "join under a base" helper. It is stated here because a caller who assumed an absolute element would
// be *rejected* would be wrong about the return value rather than about the containment, and that is a
// harder mistake to notice. If a caller needs absolute elements refused, it must check for them.
//
// Example usage:
//
//	safePath, err := SecureJoin("/var/cache", "user", filename)
//	if err != nil {
//		return fmt.Errorf("invalid path combination: %w", err)
//	}
func SecureJoin(base string, elements ...string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("base path cannot be empty")
	}

	cleanBase := filepath.Clean(base)

	// Join all elements
	fullPath := filepath.Join(append([]string{cleanBase}, elements...)...)

	// Validate the result is within base
	if !strings.HasPrefix(fullPath, cleanBase+string(filepath.Separator)) &&
		fullPath != cleanBase {
		return "", fmt.Errorf("path escapes base directory")
	}

	return fullPath, nil
}
