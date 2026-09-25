//go:build !windows

package tundevice

func ensureRouteNative(_, _ string) error     { return nil }
func ensureRouteIPv6Native(_, _ string) error { return nil }
