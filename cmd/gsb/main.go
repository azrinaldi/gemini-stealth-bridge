package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/server"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var reSafeAccountName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func init() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprint(out, "gsb — OpenAI-compatible bridge to Gemini Web (v"+config.Version+")\n\n")
		fmt.Fprint(out, "Quick start:\n")
		fmt.Fprint(out, "  gsb --login <name>                  launch isolated browser sign-in and save account\n")
		fmt.Fprint(out, "  gsb --probe                         verify accounts, models, and 5h/weekly quota\n")
		fmt.Fprint(out, "  gsb --accounts --accounts-live      print live status table (5h + weekly quota)\n")
		fmt.Fprint(out, "  gsb --web                           start daemon + Web Control Plane at http://127.0.0.1:20133/ui\n")
		fmt.Fprint(out, "  ./start.sh                          build (if needed) and start daemon with Web UI enabled\n\n")
		fmt.Fprint(out, "Remote VPS / Headless login:\n")
		fmt.Fprint(out, "  On laptop: ssh -L 20133:127.0.0.1:20133 user@vps  (and optionally `gsb --login-helper`)\n")
		fmt.Fprint(out, "  Open http://127.0.0.1:20133/ui -> '+ Add Account' -> paste Incognito cookies or launch isolated browser.\n\n")
		fmt.Fprint(out, "Flags:\n")
		flag.PrintDefaults()
	}
}

func main() {
	var (
		loginAcct        = flag.String("login", "", "Interactive browser sign-in to capture a fresh session into ~/.gemini-stealth-bridge/accounts/<name>.json")
		importChrome     = flag.String("import-chrome", "", "Import Google cookies from local Chrome/Brave/Chromium/Edge/Vivaldi/Arc/Firefox SQLite DB into accounts/<name>.json")
		cookieDB         = flag.String("cookie-db", "", "Explicit path to browser Cookies SQLite database or profile directory for --import-chrome / --login")
		chromeProfile    = flag.String("chrome-profile", "", "Browser profile name (e.g. \"Profile 4\"), profile directory, or Cookies SQLite path for --import-chrome / --login")
		importCookie     = flag.String("import-cookie", "", "Import raw Cookie header or Cookie-Editor JSON from --cookie-value, --cookie-file, or stdin into accounts/<name>.json")
		cookieValue      = flag.String("cookie-value", "", "Raw Cookie header or JSON string for --import-cookie")
		cookieFile       = flag.String("cookie-file", "", "Path to file containing raw Cookie header or Cookie-Editor JSON array for --import-cookie")
		probeOnce        = flag.Bool("probe", false, "Bootstrap all configured accounts, verify models & quota, and exit")
		rotateOnce       = flag.Bool("rotate", false, "Run dual-path cookie rotation on all configured accounts and exit")
		showVersion      = flag.Bool("version", false, "Print build version and exit")
		checkConfig      = flag.Bool("check-config", false, "Validate environment configuration and exit")
		modelsJSON       = flag.Bool("models-json", false, "Print canonical /v1/models JSON payload and exit")
		unquarantineAcct = flag.String("unquarantine", "", "Clear quarantine state for account <id> and exit")
		captureRef       = flag.Bool("capture-reference", false, "Capture reference headers from local Chrome login profile via CDP or dump instructions")
		listAccounts     = flag.Bool("accounts", false, "Print a status table for every loaded account and exit")
		accountsLive     = flag.Bool("accounts-live", false, "With --accounts, query upstream for fresh models and quota before printing")
		enableWebUI      = flag.Bool("web", false, "Enable the opt-in web configuration UI at /ui and /v1/admin/* (or set BRIDGE_WEB_UI=1)")
		runHealthcheck   = flag.Bool("healthcheck", false, "Probe local /health endpoint and exit 0 if healthy (for container healthchecks)")
		loginHelper      = flag.Bool("login-helper", false, "Run local desktop isolated-browser login helper on 127.0.0.1:20134 for remote VPS Web UI")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("gsb %s (%s/%s, %s)\n", config.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	cfg := config.Load()
	for _, w := range cfg.Warnings {
		log.Printf("[config] WARNING: %s", w)
	}
	if *enableWebUI {
		cfg.WebUI = true
	}

	if *runHealthcheck {
		hcHost := cfg.Host
		if hcHost == "" || hcHost == "0.0.0.0" || hcHost == "::" {
			hcHost = "127.0.0.1"
		}
		hcURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(hcHost, strconv.Itoa(cfg.Port)))
		hcClient := &http.Client{Timeout: 3 * time.Second}
		resp, err := hcClient.Get(hcURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
			os.Exit(1)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "healthcheck HTTP %d\n", resp.StatusCode)
			os.Exit(1)
		}
		return
	}

	if *checkConfig {
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "config validation failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("config OK")
		return
	}

	if *unquarantineAcct != "" {
		if err := runUnquarantine(cfg, *unquarantineAcct); err != nil {
			log.Fatalf("[unquarantine] failed: %v", err)
		}
		return
	}

	dbOrProfile := *cookieDB
	if dbOrProfile == "" {
		dbOrProfile = *chromeProfile
	}

	if *captureRef {
		if err := runCaptureReference(cfg, dbOrProfile); err != nil {
			log.Fatalf("[capture-reference] failed: %v", err)
		}
		return
	}

	if *listAccounts || *accountsLive {
		runAccountsTable(cfg, *accountsLive)
		return
	}

	if *modelsJSON {
		runModelsJSON(cfg)
		return
	}
	if *loginHelper {
		if err := runLocalLoginHelper(); err != nil {
			log.Fatalf("[login-helper] failed: %v", err)
		}
		return
	}

	if *loginAcct != "" {
		if err := runInteractiveLogin(cfg, *loginAcct, dbOrProfile); err != nil {
			log.Fatalf("[login] failed: %v", err)
		}
		return
	}
	if *importChrome != "" {
		if err := runImportChrome(cfg, *importChrome, dbOrProfile); err != nil {
			log.Fatalf("[import-chrome] failed: %v", err)
		}
		return
	}
	if *importCookie != "" {
		if err := runImportCookie(cfg, *importCookie, *cookieValue, *cookieFile); err != nil {
			log.Fatalf("[import-cookie] failed: %v", err)
		}
		return
	}

	lock, lockErr := config.AcquireLock(cfg.HomeDir)
	if lockErr != nil && !*probeOnce {
		log.Fatalf("[lock] %v", lockErr)
	}
	if lock != nil {
		defer func() { _ = lock.Release() }()
	}

	srv := server.New(cfg)
	loaded := srv.LoadCredentials()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *probeOnce {
		if loaded == 0 {
			log.Fatalf("[probe] no accounts found in %s", cfg.AccountsDir)
		}
		accts := srv.EnsureModels(ctx, true)
		for _, a := range accts {
			if a.ModelsError != nil {
				fmt.Printf("Account %-16s email=%-28s ERROR: %v\n", a.ID, a.Email, a.ModelsError)
				var ue *transport.UpstreamError
				if errors.As(a.ModelsError, &ue) && ue != nil && ue.Code == "account_banned" {
					if ac, ok := a.Client.(*gemini.AccountClient); ok {
						ac.SetQuarantined(true, ue.Error())
					}
				}
			} else {
				fmt.Printf("Account %-16s email=%-28s capacity=%d models=%d fails=%d\n",
					a.ID, a.Email, a.Capacity, len(a.Models), a.Fails)
				if ac, ok := a.Client.(*gemini.AccountClient); ok {
					ac.SetSurfaceStatus("geminiWeb", "ok")
				}
			}
			for _, m := range a.Models {
				fmt.Printf("  - %-24s (%s, id=%s, cap=%d)\n", m.ModelName, m.DisplayName, m.ModelID, m.Capacity)
			}
			for _, q := range a.Quota {
				if q.Unlimited {
					fmt.Printf("  quota: %-18s unlimited\n", q.Label)
				} else {
					fmt.Printf("  quota: %-18s remaining=%d/%d resets=%s\n", q.Label, q.Remaining, q.Total, q.ResetAt)
				}
			}
		}
		return
	}

	if *rotateOnce {
		if loaded == 0 {
			log.Fatalf("[rotate] no accounts found in %s", cfg.AccountsDir)
		}
		for _, a := range srv.Accounts() {
			res, err := a.Client.RotateCookies(ctx)
			if err != nil {
				fmt.Printf("Account %-16s error: %v\n", a.ID, err)
			} else {
				fmt.Printf("Account %-16s HTTP %d rotated=%v next=%s\n", a.ID, res.Status, res.Rotated, res.NextInterval)
			}
		}
		return
	}

	srv.StartBackgroundWorkers(ctx)

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		if !config.IsLoopbackHost(cfg.Host) && cfg.APIKey == "public" && len(cfg.AccountTokens) == 0 {
			log.Printf("  WARNING:   binding to non-loopback host %s with default BRIDGE_API_KEY=public; set BRIDGE_API_KEY or BRIDGE_ACCOUNT_TOKENS", cfg.Host)
		}
		log.Printf("gemini-stealth-bridge (gsb) listening on http://%s", addr)
		log.Printf("  transport: browserless Chrome uTLS + HTTP/2 (%s)", cfg.ImpersonateProfile)
		log.Printf("  data:      %s (%d account(s) loaded)", cfg.HomeDir, loaded)
		log.Printf("  models:    http://%s/v1/models", addr)
		if cfg.WebUI {
			log.Printf("  web ui:    http://%s/ui (opt-in control plane enabled)", addr)
			if !config.IsLoopbackHost(cfg.Host) && cfg.APIKey == "public" && cfg.AdminKey == "" {
				log.Printf("  WARNING:   remote access to /ui and /v1/admin/* is locked until BRIDGE_ADMIN_KEY or a non-public BRIDGE_API_KEY is configured")
			}
		}
		if loaded > 0 {
			poolDesc := cfg.APIKey
			if cfg.APIKey == "public" {
				poolDesc = "public (no auth)"
			}
			log.Printf("  keys:      pool=%s across %d account(s), strategy=%s", poolDesc, loaded, cfg.AccountStrategy)
			log.Printf("             account keys gsb-<name> pin a single account")
		}
		if loaded == 0 {
			if cfg.WebUI {
				log.Printf("  onboarding: no accounts yet — open http://%s/ui and click '+ Add Account', or run `gsb --login <name>`", addr)
			} else {
				log.Printf("  WARNING:   no credentials found — run `gsb --login <name>` to sign in and capture a session, or start with `gsb --web`")
			}
		}
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[fatal] listen %s: %v", addr, err)
		}
	}()

	<-ctx.Done()
	log.Printf("[shutdown] stopping gemini-stealth-bridge...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	srv.Pool.Reset()
}

func runImportChrome(cfg config.Config, acctName, explicitDB string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	var dbPath string
	if strings.TrimSpace(explicitDB) != "" {
		p, err := cookies.FindBrowserCookieDB(explicitDB)
		if err != nil {
			return err
		}
		dbPath = p
	} else {
		if cfg.CookieDB != "" {
			dbPath = cookies.FindChromeCookieDB(cfg.CookieDB)
		}
		if dbPath == "" {
			dbPath = cookies.FindChromeCookieDB("")
		}
		if dbPath == "" {
			return fmt.Errorf("could not locate a Chrome/Chromium-family Cookies SQLite database (known: chrome, chromium, brave, edge, vivaldi, arc); pass --chrome-profile or --cookie-db")
		}
	}
	if strings.TrimSpace(explicitDB) == "" && strings.TrimSpace(cfg.CookieDB) == "" {
		fmt.Printf("Note: No --chrome-profile or --cookie-db specified; auto-selected %s\n", dbPath)
		fmt.Println("      Pass --chrome-profile \"<Profile Name>\" to target a dedicated browser profile.")
	}
	if lockPath := cookies.DetectActiveProfileLock(dbPath); lockPath != "" {
		fmt.Printf("Note: Browser profile lock detected (%s); concurrent browser and bridge rotation on the same session can cause __Secure-1PSIDTS conflicts.\n", filepath.Base(lockPath))
	}
	hdr, err := cookies.ReadChromeCookies(dbPath)
	if err != nil {
		return err
	}
	if cookies.ExtractSAPISID(hdr) == "" {
		return fmt.Errorf("no SAPISID cookie found in %s — sign in to gemini.google.com first (e.g. `gsb --login %s`), then try again", dbPath, acctName)
	}
	if res, resErr := cookies.ReadBrowserCookies(dbPath); resErr == nil {
		if hint := cookies.SessionCookieHint(res, 7*24*time.Hour); hint != "" {
			fmt.Printf("Warning: %s in this browser profile. A profile that is not signed in to Gemini Web produces cookies that expire almost immediately.\n", hint)
			fmt.Printf("         Prefer `gsb --login %s` so you sign in first and the session is captured afterwards.\n", acctName)
		}
	}
	outPath := filepath.Join(cfg.AccountsDir, acctName+".json")
	srv := server.New(cfg)
	ac := gemini.NewAccountClient(acctName, hdr, outPath, srv.Pool, srv.Rotator)
	ac.SetProvenance(dbPath, explicitDB)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := ac.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap with imported cookies failed: %w", err)
	}
	fmt.Printf("Imported account %q (%s) -> %s\n", acctName, ac.Email(), outPath)
	printStandaloneConnectionInfo(cfg, acctName)
	return nil
}

func runImportCookie(cfg config.Config, acctName, rawVal, filePath string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	if rawVal == "" && filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		rawVal = string(b)
	}
	if rawVal == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		rawVal = string(b)
	}
	hdr, _, err := cookies.ParseImportPayload(rawVal)
	if err != nil {
		return err
	}
	if cookies.ExtractSAPISID(hdr) == "" {
		return fmt.Errorf("cookie payload does not contain SAPISID")
	}
	outPath := filepath.Join(cfg.AccountsDir, acctName+".json")
	srv := server.New(cfg)
	ac := gemini.NewAccountClient(acctName, hdr, outPath, srv.Pool, srv.Rotator)
	ac.SetProvenance(filePath, "cookie-export")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := ac.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap with imported cookies failed: %w", err)
	}
	fmt.Printf("Imported account %q (%s) -> %s\n", acctName, ac.Email(), outPath)
	printStandaloneConnectionInfo(cfg, acctName)
	return nil
}

func runInteractiveLogin(cfg config.Config, acctName, explicitDB string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	if explicitDB == "" {
		explicitDB = cfg.CookieDB
	}
	targetURL := "https://gemini.google.com/app"
	browser, profileDir := cookies.ParseProfileHint(explicitDB)
	if explicitDB == "" {
		if bin := cookies.FindBrowserBinary(browser); bin != "" && (runtime.GOOS != "linux" || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "") {
			fmt.Printf("Launching isolated %s login window for %s (prevents cookie rotation conflicts with your main browser)...\n", filepath.Base(bin), acctName)
			fmt.Println("Sign in to Google Gemini in the popup window. It will close automatically as soon as sign-in completes.")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			res, err := cookies.RunIsolatedBrowserLogin(ctx, bin)
			cancel()
			if err == nil && res != nil && res.CookieHeader != "" {
				return runImportCookie(cfg, acctName, res.CookieHeader, "")
			}
			fmt.Printf("Isolated auto-capture fallback (%v); continuing with manual prompt.\n", err)
		}
	}
	if explicitDB != "" {
		fmt.Printf("Opening %s in your browser (target profile: %s)...\n", targetURL, explicitDB)
	} else {
		fmt.Printf("Opening %s in your default browser...\n", targetURL)
	}
	openBrowserURL(targetURL, browser, profileDir)
	fmt.Println("1. Sign in to Google Gemini in an Incognito window (Ctrl+Shift+N) so it does not share a session with your daily Chrome.")
	fmt.Println("2. Paste the raw Cookie header / Cookie-Editor JSON line and press Enter (then close the Incognito window):")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "" {
		return runImportCookie(cfg, acctName, line, "")
	}
	return runImportChrome(cfg, acctName, explicitDB)
}

func runLocalLoginHelper() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/browser-login", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		res, err := cookies.RunIsolatedBrowserLogin(ctx, "")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": err.Error()},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cookie":    res.CookieHeader,
			"userAgent": res.UserAgent,
		})
	})
	log.Printf("[login-helper] listening on http://127.0.0.1:20134 for isolated desktop browser login")
	return http.ListenAndServe("127.0.0.1:20134", mux)
}

// findBrowserBinary delegates to cookies.FindBrowserBinary.
func findBrowserBinary(browser string) string {
	return cookies.FindBrowserBinary(browser)
}

func openBrowserURL(url, browser, profileDir string) {
	if profileDir != "" {
		if runtime.GOOS == "darwin" {
			appName := "Google Chrome"
			switch strings.ToLower(browser) {
			case "brave":
				appName = "Brave Browser"
			case "edge":
				appName = "Microsoft Edge"
			case "vivaldi":
				appName = "Vivaldi"
			case "chromium":
				appName = "Chromium"
			case "arc":
				appName = "Arc"
			}
			if strings.EqualFold(browser, "arc") {
				// Arc has no Chrome-style --profile-directory switch; opening
				// the URL in the app is enough.
				if err := exec.Command("open", "-a", appName, url).Start(); err == nil {
					return
				}
			}
			cmd := exec.Command("open", "-a", appName, "--args", "--profile-directory="+profileDir, url)
			if err := cmd.Start(); err == nil {
				return
			}
		} else if bin := findBrowserBinary(browser); bin != "" {
			cmd := exec.Command(bin, "--profile-directory="+profileDir, url)
			if err := cmd.Start(); err == nil {
				return
			}
		}
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func printStandaloneConnectionInfo(cfg config.Config, acctName string) {
	fmt.Printf("  base url    : http://%s:%d/v1\n", cfg.Host, cfg.Port)
	fmt.Printf("  pool key    : %s", cfg.APIKey)
	if cfg.APIKey == "public" {
		fmt.Print("  (default; no auth, routes across every loaded account)")
	} else {
		fmt.Print("  (routes across every loaded account)")
	}
	fmt.Println()
	fmt.Printf("  account key : gsb-%s (or %s)  (pins this account only, no failover)\n", acctName, acctName)
	fmt.Println("  note        : this session was captured from an already-signed-in browser.")
	fmt.Println("                If Gemini later rejects it, run `gsb --login " + acctName + "` to sign in again")
	fmt.Println("                instead of copying cookies from a browser that was never signed in.")
}

func runUnquarantine(cfg config.Config, acctID string) error {
	if !reSafeAccountName.MatchString(acctID) {
		return fmt.Errorf("invalid account id %q", acctID)
	}
	path := filepath.Join(cfg.AccountsDir, acctID+".json")
	rec, _, err := cookies.LoadAccountFile(path)
	if err != nil {
		return fmt.Errorf("load account %s: %w", path, err)
	}
	rec.Quarantined = false
	rec.QuarantineReason = ""
	if rec.Surfaces == nil {
		rec.Surfaces = make(map[string]string)
	}
	rec.Surfaces["geminiWeb"] = "ok"
	if _, err := cookies.SaveCredentialFile(path, *rec); err != nil {
		return fmt.Errorf("save account %s: %w", path, err)
	}
	fmt.Printf("Cleared quarantine state for account %q (%s)\n", acctID, path)
	return nil
}

func runModelsJSON(cfg config.Config) {
	srv := server.New(cfg)
	_ = srv.LoadCredentials()
	var models []gemini.ModelInfo
	for _, a := range srv.Accounts() {
		if len(a.Models) > 0 {
			models = append(models, a.Models...)
		}
	}
	if len(models) == 0 {
		models = []gemini.ModelInfo{
			{
				ModelID:     "2.5-pro-id",
				ModelName:   "gemini-pro",
				DisplayName: "2.5 Pro",
				Aliases:     []string{"gemini-2.5-pro"},
				Description: "Advanced reasoning and coding",
				Capacity:    2,
				ModelNumber: 1,
			},
			{
				ModelID:     "2.5-flash-id",
				ModelName:   "gemini-flash",
				DisplayName: "2.5 Flash",
				Aliases:     []string{"gemini-2.5-flash"},
				Description: "Fast everyday reasoning",
				Capacity:    2,
				ModelNumber: 2,
			},
			{
				ModelID:     "2.5-flash-lite-id",
				ModelName:   "gemini-flash-lite",
				DisplayName: "2.5 Flash Lite",
				Aliases:     []string{"gemini-2.5-flash-lite"},
				Description: "High-efficiency lightweight model",
				Capacity:    1,
				ModelNumber: 1,
			},
			{
				ModelID:     "2.0-flash-thinking-id",
				ModelName:   "gemini-thinking",
				DisplayName: "Flash Thinking",
				Aliases:     []string{"gemini-2.0-flash-thinking"},
				Description: "Extended step-by-step reasoning",
				Capacity:    2,
				ModelNumber: 3,
			},
			{
				ModelID:     "95a4c0abcba24364",
				ModelName:   "gemini-carbon",
				DisplayName: "4 Carbon",
				Aliases:     []string{"gemini-4-carbon"},
				Description: "Most complex work",
				Capacity:    2,
				ModelNumber: 4,
			},
		}
	}
	payload := srv.BuildModelsPayload(models)
	b, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(b))
}

// runAccountsTable prints the same status the HTTP API reports, so an operator can
// tell at a glance which accounts are usable without calling /v1/accounts and
// cross-referencing raw JSON fields.
func runAccountsTable(cfg config.Config, live bool) {
	srv := server.New(cfg)
	if n := srv.LoadCredentials(); n == 0 {
		fmt.Fprintf(os.Stderr, "no accounts found in %s\n", cfg.AccountsDir)
		os.Exit(1)
	}
	if live {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		srv.EnsureModels(ctx, true)
	}
	accts := srv.Accounts()
	now := time.Now()

	idW, emailW := len("ACCOUNT"), len("EMAIL")
	type row struct {
		id, email, status, reason string
		quota5h, quotaWeekly      string
		resetAt                   string
	}
	rows := make([]row, 0, len(accts))
	hasWeeklyCol := false
	for _, a := range accts {
		email, quota := server.AccountSnapshot(a)
		status, reason := server.AccountStatus(a, now)

		rem5h, tot5h := 0, 0
		q5hStr, qWeeklyStr := "-", "-"
		resetAt := "-"
		sawExplicit5h := false
		for _, q := range quota {
			if q.Synthetic || q.Unlimited || q.Total <= 0 {
				continue
			}
			if q.ID == "weekly" {
				qWeeklyStr = fmt.Sprintf("%d/%d", q.Remaining, q.Total)
				hasWeeklyCol = true
				continue
			}
			if q.ID == "5h" {
				q5hStr = fmt.Sprintf("%d/%d", q.Remaining, q.Total)
				sawExplicit5h = true
				if q.ResetAt != "" {
					resetAt = q.ResetAt
				}
				continue
			}
			if !sawExplicit5h && q.Total <= 5000 {
				rem5h += q.Remaining
				tot5h += q.Total
				if q.ResetAt != "" && resetAt == "-" {
					resetAt = q.ResetAt
				}
			}
		}
		if !sawExplicit5h && tot5h > 0 {
			q5hStr = fmt.Sprintf("%d/%d", rem5h, tot5h)
		}
		rows = append(rows, row{
			id: a.ID, email: email, status: status, reason: reason,
			quota5h: q5hStr, quotaWeekly: qWeeklyStr, resetAt: resetAt,
		})
		if len(a.ID) > idW {
			idW = len(a.ID)
		}
		if len(email) > emailW {
			emailW = len(email)
		}
	}

	if hasWeeklyCol {
		fmt.Printf("%-*s  %-*s  %-12s  %-12s  %-14s  %s\n", idW, "ACCOUNT", emailW, "EMAIL", "STATUS", "5H QUOTA", "WEEKLY QUOTA", "DETAIL")
	} else {
		fmt.Printf("%-*s  %-*s  %-12s  %-12s  %s\n", idW, "ACCOUNT", emailW, "EMAIL", "STATUS", "5H QUOTA", "DETAIL")
	}
	for _, r := range rows {
		detail := r.reason
		if detail == "" && r.resetAt != "-" {
			detail = "resets " + r.resetAt
		}
		if hasWeeklyCol {
			fmt.Printf("%-*s  %-*s  %-12s  %-12s  %-14s  %s\n", idW, r.id, emailW, r.email, r.status, r.quota5h, r.quotaWeekly, detail)
		} else {
			fmt.Printf("%-*s  %-*s  %-12s  %-12s  %s\n", idW, r.id, emailW, r.email, r.status, r.quota5h, detail)
		}
	}
}

func runCaptureReference(cfg config.Config, explicitProfile string) error {
	outPath := filepath.Join(cfg.HomeDir, "reference-fingerprint.json")
	if err := os.MkdirAll(cfg.HomeDir, 0o700); err != nil {
		return err
	}
	ref := transport.RequestDump{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Method:    "POST",
		URL:       "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate",
		Kind:      "cors",
		HeaderOrder: []string{
			"sec-ch-ua",
			"x-same-domain",
			"sec-ch-ua-mobile",
			"sec-ch-ua-full-version-list",
			"sec-ch-ua-arch",
			"sec-ch-ua-platform-version",
			"sec-ch-ua-model",
			"sec-ch-ua-bitness",
			"sec-ch-ua-wow64",
			"user-agent",
			"content-type",
			"content-length",
			"x-goog-ext-525001261-jspb",
			"x-goog-ext-525005358-jspb",
			"x-goog-ext-73010989-jspb",
			"x-goog-ext-73010990-jspb",
			"sec-ch-ua-platform",
			"accept",
			"origin",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"referer",
			"accept-encoding",
			"accept-language",
			"cookie",
			"priority",
		},
		PseudoOrder: []string{":method", ":authority", ":scheme", ":path"},
		Headers: map[string][]string{
			"accept-encoding": {"gzip, deflate, br, zstd"},
			"sec-ch-ua":       {cfg.SecCHUA},
		},
		TLSExtOrder: "fixed",
	}
	data, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote Chrome reference fingerprint (%s) -> %s\n", explicitProfile, outPath)
	fmt.Printf("Compare against a live dump with:\n  BRIDGE_DUMP_REQUEST=/tmp/gsb-dumps ./gsb --probe\n  ./scripts/diff-fingerprint.sh /tmp/gsb-dumps/req-*.json %s\n", outPath)
	return nil
}
