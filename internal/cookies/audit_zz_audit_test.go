package cookies

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Simulates a realistic Cookie-Editor export of a signed-in Gemini Web session.
func zzCookieEditorExport() string {
	type c struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
	}
	names := []string{
		"SID", "HSID", "SSID", "APISID", "SAPISID",
		"__Secure-1PSID", "__Secure-1PAPISID", "__Secure-3PSID", "__Secure-3PAPISID",
		"__Secure-1PSIDTS", "__Secure-3PSIDTS", "SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC",
		"NID", "AEC",
		// things a real browser carries that are NOT in EssentialNames
		"OSID", "LSID", "ACCOUNT_CHOOSER", "__Host-GAPS", "__Host-1PLSID", "__Host-3PLSID",
		"__Secure-BUCKET", "__Secure-OSID", "SMSV", "SEARCH_SAMESITE", "CONSENT", "SOCS",
		"1P_JAR", "SIDTS", "AID", "UV", "_gcl_au",
	}
	var list []c
	for i, n := range names {
		list = append(list, c{Name: n, Value: fmt.Sprintf("val%d", i), Domain: ".google.com"})
	}
	b, _ := json.Marshal(list)
	return string(b)
}

func TestZZAuditImportDropsNonEssentialCookies(t *testing.T) {
	raw := zzCookieEditorExport()
	hdr, _, err := ParseImportPayload(raw)
	if err != nil {
		t.Fatalf("ParseImportPayload: %v", err)
	}
	got := ParseHeader(hdr)
	for _, n := range []string{"__Secure-1PSID", "OSID", "__Secure-OSID", "LSID", "__Host-1PLSID", "__Host-3PLSID", "__Host-GAPS", "ACCOUNT_CHOOSER"} {
		if got[n] == "" {
			t.Errorf("expected session/rotation cookie %s to be preserved", n)
		}
	}
	for _, n := range []string{"__Secure-BUCKET", "SIDTS", "AID", "UV", "_gcl_au", "1P_JAR", "CONSENT", "SOCS", "SEARCH_SAMESITE", "SMSV"} {
		if _, ok := got[n]; ok {
			t.Errorf("expected tracking/non-essential cookie %s to be dropped", n)
		}
	}
}

func TestZZAuditAbsorbCannotResurrectDroppedCookie(t *testing.T) {
	hdr, _, err := ParseImportPayload(zzCookieEditorExport())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Gemini Web re-issuing session/rotation cookies alongside tracking junk via Set-Cookie.
	merged, changed := AbsorbSetCookie(hdr, []string{
		"OSID=new-osid; Path=/; Domain=.google.com",
		"LSID=new-lsid; Path=/; Domain=.google.com",
		"__Host-GAPS=new-gaps; Path=/; Secure",
		"__Secure-1PSIDTS=fresh-ts; Path=/; Secure; Domain=.google.com",
		"_gcl_au=tracking-junk; Path=/; Domain=.google.com",
		"AID=tracking-aid; Path=/; Domain=.google.com",
		"__Secure-BUCKET=bucket-junk; Path=/; Domain=.google.com",
	})
	if !changed {
		t.Fatalf("expected changed=true")
	}
	got := ParseHeader(merged)
	if got["OSID"] != "new-osid" || got["LSID"] != "new-lsid" || got["__Host-GAPS"] != "new-gaps" || got["__Secure-1PSIDTS"] != "fresh-ts" {
		t.Fatalf("expected Google session/rotation cookies updated, got: OSID=%q LSID=%q GAPS=%q PSIDTS=%q",
			got["OSID"], got["LSID"], got["__Host-GAPS"], got["__Secure-1PSIDTS"])
	}
	for _, junk := range []string{"_gcl_au", "AID", "__Secure-BUCKET"} {
		if got[junk] != "" {
			t.Fatalf("expected tracking junk %s to remain dropped, got %q", junk, got[junk])
		}
	}
}

func TestZZAuditRawHeaderImportAlsoDrops(t *testing.T) {
	raw := "SID=a; HSID=b; SSID=c; APISID=d; SAPISID=e; __Secure-1PSID=f; __Secure-3PSID=g; OSID=o; LSID=l; __Secure-1PSIDTS=h; _gcl_au=junk1; AID=junk2; __Secure-BUCKET=junk3"
	hdr, _, err := ParseImportPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := ParseHeader(hdr)
	if got["OSID"] != "o" || got["LSID"] != "l" {
		t.Fatalf("raw import dropped session cookies: OSID=%q LSID=%q", got["OSID"], got["LSID"])
	}
	for _, junk := range []string{"_gcl_au", "AID", "__Secure-BUCKET"} {
		if got[junk] != "" {
			t.Fatalf("raw import kept tracking junk %s=%q", junk, got[junk])
		}
	}
}
