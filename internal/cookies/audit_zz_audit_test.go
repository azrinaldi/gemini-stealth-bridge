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
	fmt.Printf("AUDIT surviving cookie count after import = %d\n", len(got))
	for _, n := range []string{"OSID", "LSID", "ACCOUNT_CHOOSER", "__Host-GAPS", "__Host-1PLSID", "__Secure-BUCKET", "SIDTS", "AID"} {
		_, ok := got[n]
		fmt.Printf("AUDIT import kept %-18s = %v\n", n, ok)
		if ok {
			t.Errorf("expected %s to be dropped", n)
		}
	}
	if _, ok := got["__Secure-1PSID"]; !ok {
		t.Fatalf("essential cookie lost")
	}
}

func TestZZAuditAbsorbCannotResurrectDroppedCookie(t *testing.T) {
	hdr, _, err := ParseImportPayload(zzCookieEditorExport())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Gemini Web re-issuing the full jar via Set-Cookie during a request.
	merged, changed := AbsorbSetCookie(hdr, []string{
		"OSID=new-osid; Path=/; Domain=.google.com",
		"LSID=new-lsid; Path=/; Domain=.google.com",
		"__Host-GAPS=new-gaps; Path=/; Secure",
		"__Secure-1PSIDTS=fresh-ts; Path=/; Secure; Domain=.google.com",
	})
	fmt.Printf("AUDIT absorb changed=%v hasOSID=%v hasLSID=%v hasGAPS=%v hasPSIDTS=%v\n",
		changed,
		ParseHeader(merged)["OSID"] != "",
		ParseHeader(merged)["LSID"] != "",
		ParseHeader(merged)["__Host-GAPS"] != "",
		ParseHeader(merged)["__Secure-1PSIDTS"] != "")
	if ParseHeader(merged)["OSID"] != "" || ParseHeader(merged)["LSID"] != "" || ParseHeader(merged)["__Host-GAPS"] != "" {
		t.Fatalf("unexpected: non-essential cookie absorbed")
	}
}

func TestZZAuditRawHeaderImportAlsoDrops(t *testing.T) {
	raw := "SID=a; HSID=b; SSID=c; APISID=d; SAPISID=e; __Secure-1PSID=f; __Secure-3PSID=g; OSID=o; LSID=l; __Secure-1PSIDTS=h"
	hdr, _, err := ParseImportPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("AUDIT raw-header import result = %q\n", hdr)
	if ParseHeader(hdr)["OSID"] != "" || ParseHeader(hdr)["LSID"] != "" {
		t.Fatalf("raw import kept non-essential cookies")
	}
}
