package cookies

import (
	"strings"
	"testing"
)

func TestIsolatedLoginArgsDisableDBSC(t *testing.T) {
	args := isolatedLoginArgs("/tmp/gsb-isolated-profile", 9222)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--user-data-dir=/tmp/gsb-isolated-profile",
		"--remote-debugging-port=9222",
		"--disable-features=DeviceBoundSessionCredentials",
		"https://gemini.google.com/app",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("isolated login args missing %q: %v", want, args)
		}
	}
}
