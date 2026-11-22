package cookies

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
	if strings.Count(got, "OSID=o1") != 1 || strings.Count(got, "__Secure-OSID=o2") != 1 {
		t.Fatalf("duplicate OSID entries in canonical header: %s", got)
	}
}

func TestParseHeader_StripsCookiePrefixAndMultilineHeaders(t *testing.T) {
	for _, raw := range []string{
		`Cookie: SAPISID=s1; SID=s2; OSID=o1`,
		`cookie: OSID="o1"; SAPISID=s1; SID=s2`,
		"GET /app HTTP/2\nHost: gemini.google.com\ncookie: SID=s2; SAPISID=s1\ncookie: OSID=o1\nAccept: */*",
	} {
		got := ParseHeader(raw)
		if got["SAPISID"] != "s1" || got["SID"] != "s2" || got["OSID"] != "o1" {
			t.Fatalf("ParseHeader(%q) lost cookies: %+v", raw, got)
		}
	}
}

func TestParseImportPayload_AllUserFormats(t *testing.T) {
	t.Run("curl_bash_H_cookie_and_b_flag", func(t *testing.T) {
		curlH := "curl 'https://gemini.google.com/app' \\\n  -H 'accept: */*' \\\n  -H 'cookie: SID=s1; SAPISID=sap1; OSID=o1; __Secure-1PSID=p1'"
		hdr, _, err := ParseImportPayload(curlH)
		if err != nil {
			t.Fatalf("ParseImportPayload(curl -H) failed: %v", err)
		}
		got := ParseHeader(hdr)
		if got["SID"] != "s1" || got["SAPISID"] != "sap1" || got["OSID"] != "o1" || got["__Secure-1PSID"] != "p1" {
			t.Fatalf("unexpected curl -H cookies: %+v", got)
		}

		curlB := "curl 'https://gemini.google.com/app' -b 'SAPISID=sap_b; OSID=o_b; __Secure-1PSID=p_b'"
		hdrB, _, err := ParseImportPayload(curlB)
		if err != nil {
			t.Fatalf("ParseImportPayload(curl -b) failed: %v", err)
		}
		gotB := ParseHeader(hdrB)
		if gotB["SAPISID"] != "sap_b" || gotB["OSID"] != "o_b" || gotB["__Secure-1PSID"] != "p_b" {
			t.Fatalf("unexpected curl -b cookies: %+v", gotB)
		}
	})

	t.Run("curl_cmd_caret_escaped", func(t *testing.T) {
		curlCmd := "curl ^\"https://gemini.google.com/app^\" ^\r\n  -H ^\"cookie: SID=s1; SAPISID=sap_cmd; OSID=o_cmd^\" ^\r\n  -H ^\"user-agent: Chrome^\"\r\n"
		hdr, _, err := ParseImportPayload(curlCmd)
		if err != nil {
			t.Fatalf("ParseImportPayload(curl cmd) failed: %v", err)
		}
		got := ParseHeader(hdr)
		if got["SID"] != "s1" || got["SAPISID"] != "sap_cmd" || got["OSID"] != "o_cmd" {
			t.Fatalf("unexpected curl cmd cookies: %+v", got)
		}
	})

	t.Run("devtools_application_cookies_tsv", func(t *testing.T) {
		tsv := "SAPISID\tsap_tsv\t.google.com\t/\tSession\t20\t\t✓\n" +
			"OSID\to_tsv\tgemini.google.com\t/\tSession\t20\t✓\t✓\n" +
			"__Secure-1PSID\tp_tsv\t.google.com\t/\t2030-01-01T00:00:00.000Z\t40\t✓\t✓\n"
		hdr, _, err := ParseImportPayload(tsv)
		if err != nil {
			t.Fatalf("ParseImportPayload(TSV) failed: %v", err)
		}
		got := ParseHeader(hdr)
		if got["SAPISID"] != "sap_tsv" || got["OSID"] != "o_tsv" || got["__Secure-1PSID"] != "p_tsv" {
			t.Fatalf("unexpected TSV cookies: %+v", got)
		}
	})

	t.Run("netscape_cookies_txt_with_httponly", func(t *testing.T) {
		netscape := "# Netscape HTTP Cookie File\n" +
			"# This is a generated file! Do not edit.\n\n" +
			".google.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsap_net\n" +
			"#HttpOnly_gemini.google.com\tFALSE\t/\tTRUE\t0\tOSID\to_net\n" +
			"#HttpOnly_.google.com\tTRUE\t/\tTRUE\t0\t__Secure-1PSID\tp_net\n"
		hdr, _, err := ParseImportPayload(netscape)
		if err != nil {
			t.Fatalf("ParseImportPayload(Netscape) failed: %v", err)
		}
		got := ParseHeader(hdr)
		if got["SAPISID"] != "sap_net" || got["OSID"] != "o_net" || got["__Secure-1PSID"] != "p_net" {
			t.Fatalf("unexpected Netscape cookies: %+v", got)
		}
	})

	t.Run("extension_json_without_domain_and_with_url", func(t *testing.T) {
		noDomain := `[{"name":"SAPISID","value":"sap_site"},{"name":"OSID","value":"o_site"}]`
		hdr, _, err := ParseImportPayload(noDomain)
		if err != nil {
			t.Fatalf("ParseImportPayload(no-domain JSON) failed: %v", err)
		}
		got := ParseHeader(hdr)
		if got["SAPISID"] != "sap_site" || got["OSID"] != "o_site" {
			t.Fatalf("unexpected site-scoped JSON cookies: %+v", got)
		}

		withURL := `[{"name":"SAPISID","value":"sap_url","url":"https://gemini.google.com/app"},{"name":"OSID","value":"o_url","url":"https://gemini.google.com/app"}]`
		hdrURL, _, err := ParseImportPayload(withURL)
		if err != nil {
			t.Fatalf("ParseImportPayload(url JSON) failed: %v", err)
		}
		gotURL := ParseHeader(hdrURL)
		if gotURL["SAPISID"] != "sap_url" || gotURL["OSID"] != "o_url" {
			t.Fatalf("unexpected url-scoped JSON cookies: %+v", gotURL)
		}
	})

	t.Run("extension_json_skips_expired_and_prefers_root_path", func(t *testing.T) {
		past := time.Now().Add(-24 * time.Hour).Unix()
		future := time.Now().Add(24 * time.Hour).Unix()
		payload := fmt.Sprintf(`[
			{"name":"SAPISID","value":"sap_expired","domain":".google.com","path":"/","expirationDate":%d},
			{"name":"SAPISID","value":"sap_valid","domain":".google.com","path":"/","expirationDate":%d},
			{"name":"OSID","value":"osid_subpath","domain":"gemini.google.com","path":"/u/1","expirationDate":%d},
			{"name":"OSID","value":"osid_root","domain":"gemini.google.com","path":"/","expirationDate":%d}
		]`, past, future, future, future)
		hdr, _, err := ParseImportPayload(payload)
		if err != nil {
			t.Fatalf("ParseImportPayload(expiry/path JSON) failed: %v", err)
		}
		got := ParseHeader(hdr)
		if got["SAPISID"] != "sap_valid" || got["OSID"] != "osid_root" {
			t.Fatalf("expected non-expired & root-path cookies, got: %+v", got)
		}
	})
}
