package proxy

import "testing"

// TestParseEgressIP covers the probe-response parser: JSON (api.ip.fm) and
// plain-text (api.ipify.org) shapes, plus garbage input.
func TestParseEgressIP(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"api.ip.fm json", []byte(`{"code":200,"message":"Success","data":{"ip":"203.0.113.7","country_code":"MY"}}`), "203.0.113.7"},
		{"api.ipify plain", []byte("203.0.113.9\n"), "203.0.113.9"},
		{"ipv6 json", []byte(`{"data":{"ip":"2001:db8::1"}}`), "2001:db8::1"},
		{"empty", []byte(""), ""},
		// SEC-06: non-IP content must NOT be surfaced (proxy-controlled body)
		{"garbage", []byte("not-an-ip"), ""},
		{"json garbage ip", []byte(`{"data":{"ip":"not-an-ip"}}`), ""},
		{"json no ip", []byte(`{"data":{}}`), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseEgressIP(c.in); got != c.want {
				t.Fatalf("parseEgressIP(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
