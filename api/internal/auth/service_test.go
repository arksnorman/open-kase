package auth

import "testing"

// TestThrottleClient: per-client sign-in keys name an IPv4 client by its
// address and an IPv6 client by its /64; requests without a usable client
// address keep what they have (the empty string is the shared unknown
// client).
func TestThrottleClient(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "",
		"192.0.2.1":            "192.0.2.1",
		"::ffff:192.0.2.1":     "192.0.2.1",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:DB8:1:2::1":      "2001:db8:1:2::/64",
		"not an address":       "not an address",
	} {
		if got := throttleClient(in); got != want {
			t.Errorf("throttleClient(%q) = %q, want %q", in, got, want)
		}
	}
}
