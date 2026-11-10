package kaspec

import (
	"regexp"
	"strconv"
	"strings"
)

// KaVersion is the version of the ka release using this package; ka sets
// it at startup. Empty, or not a plain release version (a development or
// pre-release build such as 1.3.0-dev), satisfies every requirement.
var KaVersion string

// UpgradeHint tells a person how to get a newer ka.
const UpgradeHint = "upgrade ka: in your ka checkout run `git fetch --tags`, check out the newest tag and run the install script again (or `brew upgrade ka`, `scoop update ka`)"

var (
	versionRe   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	versionHead = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)`)
)

// parseVersion reads the leading MAJOR.MINOR.PATCH of v.
func parseVersion(v string) ([3]int, bool) {
	m := versionHead.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range 3 {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

// VersionAtLeast reports whether version have is want or newer. A have that
// is not a plain release version (a development build) counts as newest.
func VersionAtLeast(have, want string) bool {
	w, ok := parseVersion(want)
	if !ok {
		return true
	}
	h, ok := parseVersion(have)
	if !ok || !versionRe.MatchString(strings.TrimPrefix(have, "v")) {
		return true
	}
	for i := range 3 {
		if h[i] != w[i] {
			return h[i] > w[i]
		}
	}
	return true
}

// KaVersionSatisfies reports whether this ka meets a store's requires_ka.
func KaVersionSatisfies(requires string) bool {
	return requires == "" || VersionAtLeast(KaVersion, requires)
}

// VersionAtLeastRelease is VersionAtLeast for a version that must be
// present: an empty or malformed have does not satisfy want.
func VersionAtLeastRelease(have, want string) bool {
	if _, ok := parseVersion(have); !ok {
		return false
	}
	return VersionAtLeast(have, want)
}
