package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
)

func usage() {
	fmt.Printf(`steam-ctl v%s — Steam move/uninstall without compatdata wipe (Bubble Tea TUI)

USAGE:
  steam-ctl                              Launch TUI (interactive)
  steam-ctl move <target> <appid...>     Move games to one library (batch)
  steam-ctl uninstall <appid...>         Uninstall games, keep prefix (batch)
  steam-ctl list [--json]                List games + prefix status
  steam-ctl fix-libraries                Prune stale /run/media entry

ALIASES:
  Home, Internal, Main  → Steam root
  External, Ext, Mnt    → first external library (auto-discovers next library
                           not Home; scans /mnt, /run/media, /media)
  Full paths also work: /mnt/External/SteamLibrary

OPTIONS:
  --dry-run            Show actions without executing
  --stop-steam         Auto shutdown Steam before operation
  --keep-compatdata    Keep Proton prefix (default)
  --keep-shadercache   Keep shadercache (default)
  --purge-compatdata   Actually delete compatdata on uninstall
  --purge-shadercache  Actually delete shadercache
  --no-tui             Disable interactive TUI, require explicit args
  -f, --force          Skip confirmations
  -h, --help           Show this help

EXAMPLES:
  steam-ctl
  steam-ctl move External 123456 789012 --dry-run
  steam-ctl move Home 123456
  steam-ctl uninstall 123456 789012
  steam-ctl uninstall 123456 --purge-compatdata

TUI:
  space multi-select, ctrl-a select all, / filter, enter confirm, esc back
`, version)
}

func looksLikeLibrary(o Options, s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch lower {
	case "home", "internal", "main", "steam", "external", "ext", "mnt", "":
		return true
	}
	if strings.Contains(s, "/") {
		if st, err := os.Stat(s + "/steamapps"); err == nil && st.IsDir() {
			return true
		}
		// a path-like arg is library-ish even if it doesn't exist (will error later)
		return true
	}
	if st, err := os.Stat(s + "/steamapps"); err == nil && st.IsDir() {
		return true
	}
	return false
}

func main() {
	o := defaultOptions()
	var cmd string
	var pos []string
	jsonOut := false
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "move", "uninstall", "list", "fix-libraries":
			if cmd == "" {
				cmd = a
			} else {
				pos = append(pos, a)
			}
		case "--json":
			jsonOut = true
		case "--dry-run":
			o.DryRun = true
		case "--stop-steam":
			o.StopSteam = true
		case "--keep-compatdata":
			o.PurgeCompat = false
		case "--keep-shadercache":
			o.PurgeShader = false
		case "--purge-compatdata":
			o.PurgeCompat = true
		case "--purge-shadercache":
			o.PurgeShader = true
		case "--no-tui":
			o.NoTUI = true
		case "-f", "--force":
			o.Force = true
		case "-h", "--help":
			usage()
			return
		case "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "[error] Unknown option: %s\n", a)
				usage()
				os.Exit(1)
			}
			pos = append(pos, a)
		}
	}
	// bare "list" etc. as first positional
	if cmd == "" && len(pos) > 0 {
		switch pos[0] {
		case "move", "uninstall", "list", "fix-libraries":
			cmd = pos[0]
			pos = pos[1:]
		}
	}

	if cmd == "" {
		if isTTY() && !o.NoTUI {
			if err := runTUI(o, "", nil); err != nil {
				fmt.Fprintln(os.Stderr, "[error]", err)
				os.Exit(1)
			}
			return
		}
		usage()
		os.Exit(1)
	}

	switch cmd {
	case "list":
		if jsonOut {
			printAppsJSON(o)
		} else {
			printAppsTable(o)
		}
	case "fix-libraries":
		if err := cmdFixLibraries(o); err != nil {
			fmt.Fprintln(os.Stderr, "[error]", err)
			os.Exit(1)
		}
	case "move":
		if len(pos) == 0 {
			if isTTY() && !o.NoTUI {
				if err := runTUI(o, "move", nil); err != nil {
					fmt.Fprintln(os.Stderr, "[error]", err)
					os.Exit(1)
				}
				return
			}
			fmt.Fprintln(os.Stderr, "[error] move requires <target> <appid...>")
			fmt.Fprintln(os.Stderr, "  Example: steam-ctl move External 123456")
			os.Exit(1)
		}
		if len(pos) == 1 {
			// single appid given + interactive → pick target in TUI
			if validAppID(pos[0]) && isTTY() && !o.NoTUI {
				if err := runTUI(o, "move", []string{pos[0]}); err != nil {
					fmt.Fprintln(os.Stderr, "[error]", err)
					os.Exit(1)
				}
				return
			}
			fmt.Fprintln(os.Stderr, "[error] move requires <target> <appid...>")
			fmt.Fprintln(os.Stderr, "  Example: steam-ctl move External 123456")
			os.Exit(1)
		}
		target := pos[0]
		appids := pos[1:]
		// back-compat: old order was `move <appid> <target>`
		if validAppID(target) && len(appids) == 1 && !validAppID(appids[0]) && looksLikeLibrary(o, appids[0]) {
			fmt.Fprintln(os.Stderr, "[warn] Old syntax detected (move <appid> <target>); new syntax is move <target> <appid...>. Swapping.")
			target, appids = appids[0], []string{target}
		} else {
			for _, a := range appids {
				if !validAppID(a) {
					fmt.Fprintf(os.Stderr, "[error] Invalid appid: %s\n", a)
					os.Exit(1)
				}
			}
			if validAppID(target) {
				fmt.Fprintln(os.Stderr, "[error] First arg after move must be the target library, then appids.")
				fmt.Fprintln(os.Stderr, "  Example: steam-ctl move External 123456 789012")
				os.Exit(1)
			}
		}
		failed := 0
		mctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		for _, a := range appids {
			if mctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "[warn] Cancelled — remaining games skipped.")
				os.Exit(130)
			}
			if err := moveWithContext(mctx, o, a, target, nil); err != nil {
				fmt.Fprintf(os.Stderr, "[error] move %s: %v\n", a, err)
				if mctx.Err() != nil {
					os.Exit(130)
				}
				failed++
			}
		}
		if failed > 0 {
			os.Exit(1)
		}
	case "uninstall":
		if len(pos) == 0 {
			if isTTY() && !o.NoTUI {
				if err := runTUI(o, "uninstall", nil); err != nil {
					fmt.Fprintln(os.Stderr, "[error]", err)
					os.Exit(1)
				}
				return
			}
			fmt.Fprintln(os.Stderr, "[error] uninstall requires <appid...>")
			os.Exit(1)
		}
		failed := 0
		for _, a := range pos {
			if err := cmdUninstall(o, a); err != nil {
				fmt.Fprintf(os.Stderr, "[error] uninstall %s: %v\n", a, err)
				failed++
			}
		}
		if failed > 0 {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "[error] Unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}
