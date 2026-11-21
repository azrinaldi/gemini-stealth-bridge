package cookies

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsolatedLoginArgsDisableDBSC(t *testing.T) {
	args := isolatedLoginArgs("/tmp/gsb-isolated-profile", 9222)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--user-data-dir=/tmp/gsb-isolated-profile",
		"--remote-debugging-port=9222",
		"--disable-features=DeviceBoundSessionCredentials,BoundSessionCredentials,EnableBoundSessionCredentials,DeviceBoundSessions",
		"https://gemini.google.com/app",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("isolated login args missing %q: %v", want, args)
		}
	}
}

func readWSClientFrame(br *bufio.Reader) (opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(br, hdr[:]); err != nil {
		return 0, nil, err
	}
	opcode = hdr[0] & 0x0f
	masked := (hdr[1] & 0x80) != 0
	length := uint64(hdr[1] & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

func writeWSServerFrame(w io.Writer, firstByte byte, payload []byte) error {
	hdr := []byte{firstByte}
	if len(payload) < 126 {
		hdr = append(hdr, byte(len(payload)))
	} else {
		hdr = append(hdr, 126, byte(len(payload)>>8), byte(len(payload)))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func TestCDPCallStdlibWS_FragmentedAndEventsAndPing(t *testing.T) {
	pongReceived := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", 500)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
		_ = brw.Flush()

		// Read client's request frame.
		if _, _, err := readWSClientFrame(brw.Reader); err != nil {
			return
		}

		// 1. Send an unsolicited CDP event frame (no matching id).
		_ = writeWSServerFrame(conn, 0x81, []byte(`{"method":"Network.requestWillBeSent","params":{}}`))

		// 2. Send first fragment of response (FIN=0, opcode=0x01).
		fullResp := `{"id":1,"result":{"cookies":[{"name":"SAPISID","value":"sap_ok","domain":".google.com"}]}}`
		split := len(fullResp) / 2
		_ = writeWSServerFrame(conn, 0x01, []byte(fullResp[:split]))

		// 3. Interleave a Ping control frame (opcode=0x09) mid-fragment.
		_ = writeWSServerFrame(conn, 0x89, []byte("hb"))

		// Expect client to answer with masked Pong (0x0A) before sending continuation.
		op, pongPayload, err := readWSClientFrame(brw.Reader)
		if err == nil && op == 0x0A {
			pongReceived <- string(pongPayload)
		}

		// 4. Send final continuation fragment (FIN=1, opcode=0x00).
		_ = writeWSServerFrame(conn, 0x80, []byte(fullResp[split:]))
	}))
	defer srv.Close()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/devtools/page/1"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := cdpCallStdlibWS(ctx, wsURL, `{"id":1,"method":"Network.getAllCookies"}`)
	if err != nil {
		t.Fatalf("cdpCallStdlibWS: %v", err)
	}
	if !strings.Contains(string(got), `"sap_ok"`) {
		t.Fatalf("unexpected reassembled CDP payload: %s", string(got))
	}
	select {
	case p := <-pongReceived:
		if p != "hb" {
			t.Fatalf("expected pong payload 'hb', got %q", p)
		}
	case <-time.After(time.Second):
		t.Fatal("expected Pong frame in response to server Ping")
	}
}

func TestExtractIsolatedCookies_DomainScoringAndGoogleDomainFilter(t *testing.T) {
	// Stale accounts.google.com appears BEFORE .google.com, plus an evilgoogle.com lookalike.
	raw := []domainCookie{
		{Name: "SAPISID", Value: "evil_sap", Domain: ".evilgoogle.com"},
		{Name: "SAPISID", Value: "stale_sub_sap", Domain: "accounts.google.com"},
		{Name: "SAPISID", Value: "root_sap", Domain: ".google.com"},
		{Name: "__Secure-1PSID", Value: "root_psid", Domain: ".google.com"},
		{Name: "__Secure-1PSIDTS", Value: "root_ts", Domain: ".google.com"},
		{Name: "OSID", Value: "osid_gemini", Domain: "gemini.google.com"},
		{Name: "_gcl_au", Value: "tracking_junk", Domain: ".google.com"},
	}

	hdr, baseReady, rotReady := extractIsolatedCookies(raw)
	if !baseReady || rotReady {
		t.Fatalf("expected baseReady=true rotReady=false, got base=%v rot=%v", baseReady, rotReady)
	}
	parsed := ParseHeader(hdr)
	if parsed["SAPISID"] != "root_sap" {
		t.Fatalf("expected .google.com SAPISID to beat subdomain and evilgoogle.com, got %q", parsed["SAPISID"])
	}
	if parsed["OSID"] != "osid_gemini" {
		t.Fatalf("expected OSID preserved, got %q", parsed["OSID"])
	}
	if parsed["_gcl_au"] != "" {
		t.Fatalf("expected tracking junk dropped, got %q", parsed["_gcl_au"])
	}

	// Adding rotation cookies flips rotReady=true.
	raw = append(raw,
		domainCookie{Name: "SIDCC", Value: "sidcc_1", Domain: ".google.com"},
		domainCookie{Name: "__Secure-1PSIDCC", Value: "psidcc_1", Domain: ".google.com"},
		domainCookie{Name: "__Secure-3PSIDTS", Value: "3psidts_1", Domain: ".google.com"},
	)
	_, baseReady, rotReady = extractIsolatedCookies(raw)
	if !baseReady || !rotReady {
		t.Fatalf("expected baseReady=true rotReady=true once rotation cookies settle, got base=%v rot=%v", baseReady, rotReady)
	}
}
