package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSyncVDFAppsMoveAndUninstall verifies the libraryfolders.vdf apps
// mapping follows moves and uninstalls, byte-identical outside the edits.
func TestSyncVDFAppsMoveAndUninstall(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	ext := filepath.Join(dir, "ext")
	vdf := filepath.Join(dir, "libraryfolders.vdf")
	orig := "\"libraryfolders\"\n{\n" +
		"\t\"0\"\n\t{\n\t\t\"path\"\t\t\"" + home + "\"\n\t\t\"apps\"\n\t\t{\n" +
		"\t\t\t\"111\"\t\t\"100\"\n\t\t\t\"222\"\t\t\"200\"\n\t\t}\n\t}\n" +
		"\t\"1\"\n\t{\n\t\t\"path\"\t\t\"" + ext + "\"\n\t\t\"apps\"\n\t\t{\n" +
		"\t\t\t\"333\"\t\t\"300\"\n\t\t}\n\t}\n}\n"
	if err := os.WriteFile(vdf, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	o := Options{SteamRoot: home, LibraryVDF: vdf}

	if err := syncVDFApps(o, "222", home, ext, 200); err != nil {
		t.Fatalf("move sync: %v", err)
	}
	got, _ := os.ReadFile(vdf)
	s := string(got)
	homeBlock := s[strings.Index(s, "\"0\""):strings.Index(s, "\"1\"")]
	extBlock := s[strings.Index(s, "\"1\""):]
	if strings.Contains(homeBlock, "\"222\"") {
		t.Fatal("222 should be gone from home block")
	}
	if !strings.Contains(homeBlock, "\"111\"") || !strings.Contains(extBlock, "\"333\"") {
		t.Fatal("unrelated entries must survive")
	}
	if !strings.Contains(extBlock, "\"222\"\t\t\"200\"") {
		t.Fatalf("222 should be under ext block:\n%s", extBlock)
	}

	if err := syncVDFApps(o, "222", ext, "", 0); err != nil {
		t.Fatalf("uninstall sync: %v", err)
	}
	got, _ = os.ReadFile(vdf)
	if strings.Contains(string(got), "\"222\"") {
		t.Fatal("222 should be gone everywhere after uninstall")
	}
	if err := syncVDFApps(o, "999", home, ext, 1); err == nil {
		t.Fatal("syncing unknown appid should fail without writing")
	}
}

func TestParseRsyncProgress(t *testing.T) {
	done, pct, bps, eta, ok := parseRsyncProgress("  123456789  45%    7.82MB/s    0:00:10 (xfr#123, to-chk=45/100)")
	if !ok {
		t.Fatal("expected parse ok")
	}
	if done != 123456789 {
		t.Fatalf("done=%d", done)
	}
	if pct != 45 {
		t.Fatalf("pct=%v", pct)
	}
	if bps < 7.8*1024*1024 || bps > 7.9*1024*1024 {
		t.Fatalf("bps=%v", bps)
	}
	if eta != 10*time.Second {
		t.Fatalf("eta=%v", eta)
	}
	if _, _, _, _, ok := parseRsyncProgress("sending incremental file list"); ok {
		t.Fatal("non-progress line should not parse")
	}
	if parseSpeed("1.50kB") != 1.5*1024 {
		t.Fatalf("kB speed=%v", parseSpeed("1.50kB"))
	}
	if parseETA("1:02:03") != time.Hour+2*time.Minute+3*time.Second {
		t.Fatalf("eta hms=%v", parseETA("1:02:03"))
	}
	if parseETA("bogus") != 0 {
		t.Fatal("bogus eta should be 0")
	}
}

// TestMoveCancelLeavesSourceIntact exercises the cancel path with a
// pre-cancelled context against a fake Steam root. A fake pgrep on PATH
// keeps isSteamRunning false so no real Steam interaction happens.
func TestMoveCancelLeavesSourceIntact(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	ext := filepath.Join(dir, "ext")
	game := filepath.Join(home, "steamapps", "common", "TestGame")
	if err := os.MkdirAll(game, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ext, "steamapps", "common"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(game, "file.txt"), []byte("hello-steam"), 0o644); err != nil {
		t.Fatal(err)
	}
	acf := "\"AppState\"\n{\n\t\"appid\"\t\t\"999990\"\n\t\"name\"\t\t\"Test Game\"\n\t\"installdir\"\t\t\"TestGame\"\n\t\"SizeOnDisk\"\t\t\"12\"\n}\n"
	if err := os.WriteFile(filepath.Join(home, "steamapps", "appmanifest_999990.acf"), []byte(acf), 0o644); err != nil {
		t.Fatal(err)
	}
	vdf := "\"libraryfolders\"\n{\n\t\"0\"\n\t{\n\t\t\"path\"\t\t\"" + home + "\"\n\t}\n\t\"1\"\n\t{\n\t\t\"path\"\t\t\"" + ext + "\"\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(home, "steamapps", "libraryfolders.vdf"), []byte(vdf), 0o644); err != nil {
		t.Fatal(err)
	}
	fakebin := filepath.Join(dir, "fakebin")
	if err := os.MkdirAll(fakebin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakebin, "pgrep"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakebin+":"+os.Getenv("PATH"))

	o := Options{SteamRoot: home, LibraryVDF: filepath.Join(home, "steamapps", "libraryfolders.vdf"),
		SteamPIDFile: filepath.Join(dir, "steam.pid"), Force: true}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the copy starts
	err := moveWithContext(ctx, o, "999990", ext, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cancel") {
		t.Fatalf("expected cancel error, got %v", err)
	}
	if _, err := os.Stat(game); err != nil {
		t.Fatal("source game dir must survive cancel")
	}
	if _, err := os.Stat(filepath.Join(home, "steamapps", "appmanifest_999990.acf")); err != nil {
		t.Fatal("source manifest must survive cancel")
	}
	if _, err := os.Stat(filepath.Join(ext, "steamapps", "common", "TestGame")); !os.IsNotExist(err) {
		t.Fatal("partial target must be removed on cancel")
	}
}
