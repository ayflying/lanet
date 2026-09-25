//go:build !windows

package tundevice

func configureAddressNative(string, string, int) error     { return nil }
func configureAddressIPv6Native(string, string, int) error { return nil }
func configureInterfaceNative(string) error                { return nil }
