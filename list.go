package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func printAppsTable(o Options) {
	fmt.Println("[steam-ctl] Scanning libraries...")
	fmt.Println()
	apps := scanApps(o)
	wAppID, wSize, wPrefix, wShader, wLib, wName := 8, 9, 10, 8, 12, 34
	fmt.Printf("\033[1m%-*s %-*s %-*s %-*s %-*s %-*s\033[0m\n", wAppID, "APPID", wSize, "SIZE", wPrefix, "PREFIX", wShader, "SHADER", wLib, "LIBRARY", wName, "NAME")
	fmt.Printf("%s %s %s %s %s %s\n", strings.Repeat("-", wAppID), strings.Repeat("-", wSize), strings.Repeat("-", wPrefix), strings.Repeat("-", wShader), strings.Repeat("-", wLib), strings.Repeat("-", wName))
	for _, a := range apps {
		prefix, shader := "none", "none"
		if a.HasPrefix {
			prefix = "keep ✓"
		}
		if a.HasShader {
			shader = "keep"
		}
		fmt.Printf("%-*s %-*s %-*s %-*s %-*s %-*s\n",
			wAppID, trunc(a.AppID, wAppID),
			wSize, trunc(a.HumanSize, wSize),
			wPrefix, prefix,
			wShader, shader,
			wLib, trunc(a.ShortLib, wLib),
			wName, trunc(a.Name, wName))
	}
	fmt.Println()
	fmt.Printf("[steam-ctl] Compatdata: %s\n", compatdataPath(o, ""))
	fmt.Println("[steam-ctl] Prefix is preserved on move/uninstall unless --purge-compatdata.")
}

func printAppsJSON(o Options) {
	apps := scanApps(o)
	type J struct {
		AppID      string `json:"appid"`
		Name       string `json:"name"`
		InstallDir string `json:"installdir"`
		Size       int64  `json:"size_bytes"`
		HumanSize  string `json:"size"`
		Library    string `json:"library"`
		ShortLib   string `json:"shortlib"`
		HasPrefix  bool   `json:"has_prefix"`
		HasShader  bool   `json:"has_shader"`
	}
	out := make([]J, 0, len(apps))
	for _, a := range apps {
		out = append(out, J{a.AppID, a.Name, a.InstallDir, a.Size, a.HumanSize, a.Library, a.ShortLib, a.HasPrefix, a.HasShader})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
