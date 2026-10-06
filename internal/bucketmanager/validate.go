package bucketmanager

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// validate.go — the bucket-name rules for the whole process.
//
// WHY THE RULE IS DUPLICATED (and not shared): the S3 copy of the naming rule
// lives in internal/frontend/s3/bucket_handlers.go (validateBucketName,
// unexported, exported for tests as s3.ValidateBucketName). bucketmanager
// cannot import the s3 package — s3 imports bucketmanager, so that is an import
// cycle — and the S3 frontend is outside this fix's file ownership, so its copy
// cannot be redirected here. The rule is therefore duplicated, and the two
// copies are pinned together by a MACHINE CHECK rather than by a comment:
// TestValidateNameMatchesS3Rule (validate_sync_test.go, external test package)
// compares this copy against s3.ValidateBucketName over a corpus of names, and
// fails on any divergence in accept/reject OR in the error text. If you change
// one copy, run that test, and change the other.
//
// WHY VALIDATION LIVES HERE AT ALL: bucketmanager is the shared entry point BOTH
// frontends call for bucket lifecycle. The rule used to be reached only through
// the s3 frontend's own validBucket gate, so the management API's
// adminCreateBucketService / adminDeleteBucketService called Create/Delete with
// NO gate: a ".." segment survived filepath.Join's cleaning and Delete removed a
// directory OUTSIDE the data root (bughunt H3). Validating here covers every
// caller, present and future.
//
// THE TWO LAYERS, and why the containment layer is not exempt:
//
//  1. checkContainment is UNCONDITIONAL. A name must be one path segment that
//     cannot climb: non-empty, not "." or "..", and free of any separator. No
//     configuration can waive this — a custom bucket's PATH comes from config,
//     but its NAME still reaches filepath.Join on the data-root path and must
//     never be able to address anything but a direct child. This is what makes
//     "no name, custom or not, can delete outside the data root" TOTAL.
//
//  2. validateNameRule is the S3 naming rule (3-63 chars, lowercase/digits/
//     hyphens/periods, no consecutive periods, alphanumeric edges, not an IP).
//     It IS waived for a config-declared custom bucket, mirroring the s3
//     frontend's validBucket exemption so the S3 wire stays byte-identical for
//     names the operator legitimately declared (e.g. "weird_bucket_NAME",
//     covered by s3's own validBucket custom-exemption test).

// ValidateName applies BOTH layers to name with NO custom-bucket exemption. It
// is the bare rule: containment plus the S3 naming rule. Callers that must
// preserve the S3 wire semantics of a config-declared custom bucket use
// Env.validateName instead.
func ValidateName(name string) error {
	if err := checkContainment(name); err != nil {
		return err
	}
	return validateNameRule(name)
}

// checkContainment is the unconditional, non-waivable layer: name must be a
// single path segment that cannot address anything but a direct child of the
// resolved bucket root. It rejects "", ".", "..", any name carrying a path
// separator, and any name whose cleaning would climb out of the root.
func checkContainment(name string) error {
	switch name {
	case "":
		return fmt.Errorf("bucket name must not be empty")
	case ".", "..":
		return fmt.Errorf("bucket name %q must not be a relative path segment", name)
	}
	// Both separators are checked, not just the host's: a config file (or a
	// peer frontend) written on Windows must not smuggle a backslash onto a
	// POSIX path where filepath.Join would treat it as an ordinary character
	// and let it pass as one long bucket directory name.
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("bucket name must not contain a path separator")
	}
	return nil
}

// validateNameRule is the S3 bucket naming rule: 3-63 characters, lowercase
// letters/digits/hyphens/periods only, no consecutive periods, alphanumeric
// edges, and never an IP address. The character set admits no separator and no
// leading dot, so every name it accepts is also containment-safe.
func validateNameRule(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name must be between 3 and 63 characters")
	}
	// Must start with lowercase letter or number
	if (name[0] < 'a' || name[0] > 'z') && (name[0] < '0' || name[0] > '9') {
		return fmt.Errorf("bucket name must start with a lowercase letter or number")
	}
	// Must end with lowercase letter or number
	last := name[len(name)-1]
	if (last < 'a' || last > 'z') && (last < '0' || last > '9') {
		return fmt.Errorf("bucket name must end with a lowercase letter or number")
	}
	// Check valid characters and no consecutive periods
	prevChar := byte(0)
	for i := range len(name) {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return fmt.Errorf("bucket name can only contain lowercase letters, numbers, hyphens, and periods")
		}
		if c == '.' && prevChar == '.' {
			return fmt.Errorf("bucket name cannot have consecutive periods")
		}
		prevChar = c
	}
	// Cannot be formatted as IP address
	if regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`).MatchString(name) {
		return fmt.Errorf("bucket name cannot be formatted as an IP address")
	}
	return nil
}

// validateName is the entry-point guard applied by Create and Delete BEFORE any
// path is resolved, any stat/read runs, and any provisioner is asked to create
// or destroy anything. It returns an *objectmodel.Error carrying 400, so the S3
// frontend would render InvalidArgument/400 and the management API
// {"error":{"code":"InvalidArgument",...}} with status 400 — never a 500: the
// request was refused on its face and nothing was touched.
func (e Env) validateName(name string) error {
	if err := checkContainment(name); err != nil {
		logInvalidBucketName(name, err)
		return invalidNameError(err)
	}
	// Config-declared custom buckets are exempt from the S3 naming rules only
	// (operator-controlled names, e.g. "weird_bucket_NAME"), exactly as the s3
	// frontend's validBucket exempts them — never from containment.
	if _, isCustom := e.Custom(name); isCustom {
		return nil
	}
	if err := validateNameRule(name); err != nil {
		logInvalidBucketName(name, err)
		return invalidNameError(err)
	}
	return nil
}

// invalidNameError renders a rule violation as the shared 400 taxonomy error.
func invalidNameError(err error) error {
	return objectmodel.NewError(objectmodel.CodeInvalidArgument,
		"The requested bucket name is invalid: "+err.Error(), 400)
}

// logInvalidBucketName records the rejection (quote guards the log line; the
// name is caller-supplied).
func logInvalidBucketName(name string, err error) {
	log.Printf("Refusing bucket name %s: %v", strconv.Quote(name), err)
}
