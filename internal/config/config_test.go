package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestConfigCloneDeepCopiesReferenceFields(t *testing.T) {
	orig := Config{
		Host:             "127.0.0.1",
		Port:             20133,
		AccountTokens:    map[string]string{"tok1": "acct1"},
		GroupTokens:      map[string]string{"grp1": "team1"},
		AccountGroups:    map[string]string{"acct1": "team1"},
		AccountPriority:  []string{"acct1", "acct2"},
		EgressPerAccount: map[string]string{"acct1": "socks5://127.0.0.1:1080"},
		FieldSources:     map[string]string{"BRIDGE_ACCOUNT_STRATEGY": "env"},
		Warnings:         []string{"warn1"},
	}

	cloned := orig.Clone()

	cloned.AccountTokens["tok2"] = "acct2"
	cloned.GroupTokens["grp2"] = "team2"
	cloned.AccountGroups["acct2"] = "team2"
	cloned.AccountPriority[0] = "acct99"
	cloned.EgressPerAccount["acct2"] = "http://proxy:8080"
	cloned.FieldSources["BRIDGE_ACCOUNT_STRATEGY"] = "file"
	cloned.Warnings[0] = "mutated"

	if len(orig.AccountTokens) != 1 || orig.AccountTokens["tok1"] != "acct1" {
		t.Fatalf("AccountTokens mutated on original: %+v", orig.AccountTokens)
	}
	if len(orig.GroupTokens) != 1 || orig.GroupTokens["grp1"] != "team1" {
		t.Fatalf("GroupTokens mutated on original: %+v", orig.GroupTokens)
	}
	if len(orig.AccountGroups) != 1 || orig.AccountGroups["acct1"] != "team1" {
		t.Fatalf("AccountGroups mutated on original: %+v", orig.AccountGroups)
	}
	if orig.AccountPriority[0] != "acct1" {
		t.Fatalf("AccountPriority mutated on original: %+v", orig.AccountPriority)
	}
	if len(orig.EgressPerAccount) != 1 || orig.EgressPerAccount["acct1"] != "socks5://127.0.0.1:1080" {
		t.Fatalf("EgressPerAccount mutated on original: %+v", orig.EgressPerAccount)
	}
	if orig.FieldSources["BRIDGE_ACCOUNT_STRATEGY"] != "env" {
		t.Fatalf("FieldSources mutated on original: %+v", orig.FieldSources)
	}
	if orig.Warnings[0] != "warn1" {
		t.Fatalf("Warnings mutated on original: %+v", orig.Warnings)
	}
}

func TestSaveEnvFileMergesConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	legacyTemp := path + ".tmp"
	if err := os.WriteFile(legacyTemp, []byte("keep sibling"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveEnvFile(path, map[string]string{"EXISTING": "keep"}); err != nil {
		t.Fatal(err)
	}
	const writers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("KEY_%d", i)
			if err := SaveEnvFile(path, map[string]string{key: "saved"}); err != nil {
				t.Errorf("save %s: %v", key, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	values := LoadEnvFile(path)
	if len(values) != writers+1 || values["EXISTING"] != "keep" {
		t.Fatalf("concurrent saves lost updates: %v", values)
	}
	for i := range writers {
		key := fmt.Sprintf("KEY_%d", i)
		if values[key] != "saved" {
			t.Errorf("%s = %q, want saved", key, values[key])
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("file mode = %04o, want 0600", got)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Errorf("expected only config.env and existing sibling, got %v, %v", entries, err)
	}
	if got, err := os.ReadFile(legacyTemp); err != nil || string(got) != "keep sibling" {
		t.Errorf("existing sibling changed: %q, %v", got, err)
	}
}

func TestSaveEnvFileCleansTempOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveEnvFile(path, map[string]string{"KEY": "value"}); err == nil {
		t.Fatal("expected saving over a directory to fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("expected only original directory after failed rename, got %v, %v", entries, err)
	}
}
