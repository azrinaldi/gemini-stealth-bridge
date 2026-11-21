package gemini

import (
	"strings"
	"testing"
)

func TestZZAuditClassifyRedirect(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		headers        map[string]string
		body           string
		wantAuth       bool
		wantStatus     int
		wantCode       string
		wantInMsg      string
		wantNotInMsg   string
	}{
		{
			name:         "ordinary app redirect hl=id",
			status:       302,
			headers:      map[string]string{"location": "https://gemini.google.com/app?hl=id&secret=q1"},
			wantAuth:     false,
			wantStatus:   302,
			wantCode:     "upstream_http_error",
			wantInMsg:    "https://gemini.google.com/app",
			wantNotInMsg: "secret=q1",
		},
		{
			name:         "regional redirect",
			status:       307,
			headers:      map[string]string{"Location": "https://gemini.google.com/app?gl=ID"},
			wantAuth:     false,
			wantStatus:   307,
			wantCode:     "upstream_http_error",
			wantInMsg:    "https://gemini.google.com/app",
			wantNotInMsg: "gl=ID",
		},
		{
			name:       "u/1 surface redirect",
			status:     302,
			headers:    map[string]string{"location": "https://gemini.google.com/u/1/app"},
			wantAuth:   false,
			wantStatus: 302,
			wantCode:   "upstream_http_error",
			wantInMsg:  "https://gemini.google.com/u/1/app",
		},
		{
			name:         "redirect to consent",
			status:       302,
			headers:      map[string]string{"location": "https://consent.google.com/ml?continue=x"},
			wantAuth:     false,
			wantStatus:   302,
			wantCode:     "upstream_http_error",
			wantInMsg:    "https://consent.google.com/ml",
			wantNotInMsg: "continue=x",
		},
		{
			name:         "consent with continue=accounts.google.com",
			status:       302,
			headers:      map[string]string{"location": "https://consent.google.com/ml?continue=https://accounts.google.com/ServiceLogin"},
			wantAuth:     false,
			wantStatus:   302,
			wantCode:     "upstream_http_error",
			wantInMsg:    "https://consent.google.com/ml",
			wantNotInMsg: "ServiceLogin",
		},
		{
			name:         "genuine signin redirect",
			status:       302,
			headers:      map[string]string{"location": "https://accounts.google.com/ServiceLogin?continue=x&secret=abc"},
			wantAuth:     true,
			wantStatus:   401,
			wantCode:     "auth_redirect",
			wantInMsg:    "accounts.google.com/ServiceLogin",
			wantNotInMsg: "secret=abc",
		},
		{
			name:         "v3 signin identifier redirect",
			status:       302,
			headers:      map[string]string{"Location": "https://accounts.google.com/v3/signin/identifier?dsh=xyz"},
			wantAuth:     true,
			wantStatus:   401,
			wantCode:     "auth_redirect",
			wantInMsg:    "accounts.google.com/v3/signin/identifier",
			wantNotInMsg: "dsh=xyz",
		},
		{
			name:       "302 no location",
			status:     302,
			headers:    map[string]string{},
			wantAuth:   false,
			wantStatus: 302,
			wantCode:   "upstream_http_error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ue := classifyUpstreamAuthOrBan(c.status, c.headers, c.body, "upstream_http_error")
			if ue.StatusCode != c.wantStatus || ue.Code != c.wantCode {
				t.Fatalf("got http=%d code=%q msg=%q, want http=%d code=%q",
					ue.StatusCode, ue.Code, ue.Message, c.wantStatus, c.wantCode)
			}
			if !c.wantAuth && ue.Code == "auth_redirect" {
				t.Fatalf("unexpected auth_redirect for non-sign-in redirect: %+v", ue)
			}
			if c.wantInMsg != "" && !strings.Contains(ue.Message, c.wantInMsg) {
				t.Fatalf("expected message %q to contain %q", ue.Message, c.wantInMsg)
			}
			if c.wantNotInMsg != "" && strings.Contains(ue.Message, c.wantNotInMsg) {
				t.Fatalf("expected message %q NOT to leak query param %q", ue.Message, c.wantNotInMsg)
			}
		})
	}
}
