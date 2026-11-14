package winc

import "testing"

func TestRedactProxyURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "http credentials", in: "http://user:pass@example.com", want: "http://***:***@example.com"},
		{name: "https credentials", in: "https://user:pass@example.com", want: "https://***:***@example.com"},
		{name: "password with at", in: "http://user:p@ss@example.com", want: "http://***:***@example.com"},
		{name: "path is not userinfo", in: "http://example.com/path:x@y", want: "http://example.com/path:x@y"},
		{name: "surrounding text", in: "before http://user:pass@example.com after", want: "before http://***:***@example.com after"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := redactProxyURL(test.in); got != test.want {
				t.Fatalf("redactProxyURL(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestProxyValue(t *testing.T) {
	values := map[string]interface{}{"HTTP_PROXY": "http://proxy.example.com", "NO_PROXY": nil}
	if got := proxyValue(values, "HTTP_PROXY"); got != "http://proxy.example.com" {
		t.Fatalf("proxyValue present = %q", got)
	}
	if got := proxyValue(values, "HTTPS_PROXY"); got != "" {
		t.Fatalf("proxyValue missing = %q", got)
	}
	if got := proxyValue(values, "NO_PROXY"); got != "" {
		t.Fatalf("proxyValue nil = %q", got)
	}
}
