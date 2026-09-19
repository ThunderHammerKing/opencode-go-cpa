//go:build !debug

package plugin

// debugTrace is compiled as an empty no-op stub for release/production builds.
func debugTrace(string, ...any) {}

// debugBodyMeta returns an empty string in release/production builds.
func debugBodyMeta([]byte) string { return "" }
