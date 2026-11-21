package cookies

import (
	"strings"
	"testing"
)

// Gemini Web now requires OSID / __Secure-OSID in addition to the SID family.
// Without them /app answers 302 to accounts.google.com/ServiceLogin even though
// the captured session is valid, and the bootstrap reports it as an expired
// session ("upstream redirected to Google sign-in").
func TestCanonicalHeaderKeepsOSID(t *testing.T) {
	raw := "SID=s1; HSID=h1; SAPISID=sap1; OSID=o1; __Secure-OSID=o2; __Secure-1PSID=p1"

	got := CanonicalHeader(ParseHeader(raw))
	for _, want := range []string{"OSID=o1", "__Secure-OSID=o2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("canonical header dropped %q: %s", want, got)
		}
	}
}

func TestCanonicalHeaderOSIDOrder(t *testing.T) {
	raw := "__Secure-OSID=o2; SID=s1; OSID=o1"
	got := CanonicalHeader(ParseHeader(raw))

	sid := strings.Index(got, "SID=s1")
	osid := strings.Index(got, "OSID=o1")
	secureOSID := strings.Index(got, "__Secure-OSID=o2")
	if sid < 0 || osid < 0 || secureOSID < 0 {
		t.Fatalf("missing cookie in canonical header: %s", got)
	}
	if osid < sid || secureOSID < sid {
		t.Fatalf("OSID cookies must follow the SID family, got: %s", got)
	}
}
