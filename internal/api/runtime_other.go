//go:build !unix

package api

// checkPrivateDir accepts the directory as it is. On Windows the fallback
// lives in the per-user temporary directory (%TEMP% is under the user's
// profile and not shared), and ownership is an ACL, not a uid and mode.
func checkPrivateDir(string) error { return nil }
