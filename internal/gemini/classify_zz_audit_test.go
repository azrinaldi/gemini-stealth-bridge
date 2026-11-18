package gemini

import (
	"fmt"
	"testing"
)

func TestZZAuditClassifyRedirect(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
	}{
		{"ordinary app redirect hl=id", 302, map[string]string{"location": "https://gemini.google.com/app?hl=id"}, ""},
		{"regional redirect", 307, map[string]string{"location": "https://gemini.google.com/app?gl=ID"}, ""},
		{"u/1 surface redirect", 302, map[string]string{"location": "https://gemini.google.com/u/1/app"}, ""},
		{"redirect to consent", 302, map[string]string{"location": "https://consent.google.com/ml?continue=x"}, ""},
		{"genuine signin redirect", 302, map[string]string{"location": "https://accounts.google.com/ServiceLogin?continue=x"}, ""},
		{"302 no location", 302, map[string]string{}, ""},
	}
	for _, c := range cases {
		ue := classifyUpstreamAuthOrBan(c.status, c.headers, c.body, "upstream_http_error")
		authMarked := "NO"
		if ue.StatusCode == 401 && ue.Code == "auth_redirect" {
			authMarked = "YES -> reported as session death"
		}
		fmt.Printf("AUDIT %-30s http=%d code=%-16s msg=%q classified_as_auth=%s\n",
			c.name, ue.StatusCode, ue.Code, ue.Message, authMarked)
	}
}
