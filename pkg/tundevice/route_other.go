//go:build !windows

package tundevice

func ensureRouteNative(_, _ string) error { return nil }
