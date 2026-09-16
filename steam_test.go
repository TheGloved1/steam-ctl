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
