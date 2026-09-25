package tundevice

import "testing"

func TestValidateULAIPv6(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{name: "Lanet ULA", ip: "fd00:6c61:6e65::1", want: true},
		{name: "Lanet ULA subnet", ip: "fd00:6c61:6e65:1::1", want: true},
		{name: "other ULA", ip: "fd12:3456:789a::1"},
		{name: "ULA outside project prefix", ip: "fd00:6c61:6e64::1"},
		{name: "global unicast", ip: "2001:db8::1"},
		{name: "link local", ip: "fe80::1"},
		{name: "IPv4", ip: "10.7.0.1"},
		{name: "IPv4 mapped", ip: "::ffff:10.7.0.1"},
		{name: "zone", ip: "fd12::1%tun0"},
		{name: "invalid", ip: "not-an-ip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateULAIPv6(tt.ip)
			if (err == nil) != tt.want {
				t.Fatalf("validateULAIPv6(%q) error = %v, want valid %v", tt.ip, err, tt.want)
			}
		})
	}
}
