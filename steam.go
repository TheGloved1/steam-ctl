package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options carries CLI flags + runtime switches.
type Options struct {
	DryRun       bool
	StopSteam    bool
	PurgeCompat  bool
	PurgeShader  bool
	Force        bool
	NoTUI        bool
	Quiet        bool // suppress rsync progress (TUI run screen)
	SteamRoot    string
	LibraryVDF   string
	SteamPIDFile string
}

var version = "2.0.0"

func defaultOptions() Options {
	root := detectSteamRoot()
	home, _ := os.UserHomeDir()
	return Options{
		SteamRoot:    root,
		LibraryVDF:   filepath.Join(root, "steamapps", "libraryfolders.vdf"),
		SteamPIDFile: filepath.Join(home, ".steam", "steam.pid"),
	}
}

// ---------- discovery ----------

func detectSteamRoot() string {
	if v := os.Getenv("STEAM_ROOT"); v != "" {
		if st, err := os.Stat(filepath.Join(v, "steamapps")); err == nil && st.IsDir() {
			return v
		}
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, ".local/share/Steam"),
		filepath.Join(home, ".steam/steam"),
		filepath.Join(home, ".steam/root"),
		filepath.Join(home, ".steam"),
	} {
		if st, err := os.Stat(filepath.Join(c, "steamapps")); err == nil && st.IsDir() {
			if rp, err := filepath.EvalSymlinks(c); err == nil {
				return rp
			}
			return c
		}
		if rp, err := filepath.EvalSymlinks(c); err == nil {
			if st, err := os.Stat(filepath.Join(rp, "steamapps")); err == nil && st.IsDir() {
				return rp
			}
			if st, err := os.Stat(filepath.Join(rp, "steam", "steamapps")); err == nil && st.IsDir() {
				return filepath.Join(rp, "steam")
			}
		}
	}
	// fallback: locate libraryfolders.vdf
	for _, base := range []string{filepath.Join(home, ".local/share"), filepath.Join(home, ".steam")} {
		var found string
		_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
			if err != nil || found != "" {
				return nil
			}
			if !info.IsDir() && info.Name() == "libraryfolders.vdf" {
				found = p
			}
			return nil
		})
		if found != "" {
			return filepath.Dir(filepath.Dir(found))
		}
	}
	return filepath.Join(home, ".local/share/Steam")
}

var vdfPathRe = regexp.MustCompile(`"path"\s+"([^"]+)"`)

func getLibraries(o Options) []string {
	var libs []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		ap, err := filepath.Abs(p)
		if err != nil {
			ap = p
		}
		if rp, err := filepath.EvalSymlinks(ap); err == nil {
			ap = rp
		}
		if !seen[ap] {
			seen[ap] = true
			libs = append(libs, ap)
		}
	}
	if data, err := os.ReadFile(o.LibraryVDF); err == nil {
		for _, m := range vdfPathRe.FindAllStringSubmatch(string(data), -1) {
			add(m[1])
		}
	}
	add(o.SteamRoot)
	return libs
}

// resolveExternal returns the first library that isn't the Steam root and has
// a steamapps dir, else scans common mount points for a SteamLibrary.
func resolveExternal(o Options) string {
	libs := getLibraries(o)
	rootRP, _ := filepath.EvalSymlinks(o.SteamRoot)
	for _, l := range libs {
		lrp, _ := filepath.EvalSymlinks(l)
		if lrp == "" {
			lrp = l
		}
		if lrp != rootRP {
			if st, err := os.Stat(filepath.Join(l, "steamapps")); err == nil && st.IsDir() {
				return l
			}
		}
	}
	for _, base := range []string{"/mnt", "/run/media", "/media"} {
		var found string
		_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
			if err != nil || found != "" {
				return nil
			}
			depth := strings.Count(strings.TrimPrefix(p, base), string(os.PathSeparator))
			if depth > 4 {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if info.IsDir() && info.Name() == "SteamLibrary" {
				found = p
				return filepath.SkipDir
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	return o.SteamRoot
}

func resolveLibraryAlias(o Options, input string) (string, error) {
	lower := strings.ToLower(strings.TrimSpace(input))
	switch lower {
	case "home", "internal", "main", "steam":
		return o.SteamRoot, nil
	case "external", "ext", "mnt", "":
		return resolveExternal(o), nil
	}
	if strings.Contains(input, "/") {
		ap, err := filepath.Abs(input)
		if err != nil {
			return input, fmt.Errorf("cannot resolve %q", input)
		}
		return ap, nil
	}
	if st, err := os.Stat(filepath.Join(input, "steamapps")); err == nil && st.IsDir() {
		ap, _ := filepath.Abs(input)
		return ap, nil
	}
	return input, fmt.Errorf("unknown library %q (use Home, External, or a full path)", input)
}

func findSourceLib(o Options, appid string) (string, error) {
	for _, lib := range getLibraries(o) {
		if _, err := os.Stat(filepath.Join(lib, "steamapps", "appmanifest_"+appid+".acf")); err == nil {
			return lib, nil
		}
	}
	return "", fmt.Errorf("app %s not found in any library", appid)
}

var manifestFieldRe = map[string]*regexp.Regexp{}

func parseManifestField(manifest, field string) string {
	re, ok := manifestFieldRe[field]
	if !ok {
		re = regexp.MustCompile(`"` + regexp.QuoteMeta(field) + `"\s+"([^"]+)"`)
		manifestFieldRe[field] = re
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		return ""
	}
	if m := re.FindStringSubmatch(string(data)); m != nil {
		return m[1]
	}
	return ""
}

// ---------- app model ----------

// App is one installed game + prefix status.
type App struct {
	AppID      string
	Name       string
	InstallDir string
	Size       int64
	HumanSize  string
	Library    string
	ShortLib   string
	HasPrefix  bool
	HasShader  bool
	PrefixPath string
}

func shortLibName(o Options, lib string) string {
	lrp, _ := filepath.EvalSymlinks(lib)
	rrp, _ := filepath.EvalSymlinks(o.SteamRoot)
	if lrp == "" {
		lrp = lib
	}
	if rrp == "" {
		rrp = o.SteamRoot
	}
	if lrp == rrp {
		return "Home"
	}
	base := filepath.Base(lib)
	if base == "SteamLibrary" {
		parent := filepath.Base(filepath.Dir(lib))
		if parent != "" && parent != "/" && parent != "." {
			return parent
		}
		return "External"
	}
	if base == "" {
		return "Ext"
	}
	return base
}

func compatdataPath(o Options, appid string) string {
	for _, lib := range getLibraries(o) {
		p := filepath.Join(lib, "steamapps", "compatdata", appid)
		if st, err := os.Stat(p); err == nil && (st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
			if rp, err := filepath.EvalSymlinks(p); err == nil {
				return rp
			}
			return p
		}
	}
	return filepath.Join(o.SteamRoot, "steamapps", "compatdata", appid)
}

func shaderLibs(o Options, appid string) []string {
	var out []string
	for _, lib := range getLibraries(o) {
		if st, err := os.Stat(filepath.Join(lib, "steamapps", "shadercache", appid)); err == nil && st.IsDir() {
			out = append(out, lib)
		}
	}
	return out
}

func humanSize(n int64) string {
	if n <= 0 {
		return "—"
	}
	units := []string{"B", "K", "M", "G", "T"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}

func scanApps(o Options) []App {
	seen := map[string]bool{}
	var apps []App
	for _, lib := range getLibraries(o) {
		matches, _ := filepath.Glob(filepath.Join(lib, "steamapps", "appmanifest_*.acf"))
		for _, m := range matches {
			base := filepath.Base(m)
			digits := ""
			for _, c := range base {
				if c >= '0' && c <= '9' {
					digits += string(c)
				}
			}
			// digits includes full number; extract via manifest glob name
			id := strings.TrimSuffix(strings.TrimPrefix(base, "appmanifest_"), ".acf")
			if _, err := strconv.Atoi(id); err != nil {
				continue
			}
			_ = digits
			if seen[id] {
				continue
			}
			seen[id] = true
			name := parseManifestField(m, "name")
			installdir := parseManifestField(m, "installdir")
			var size int64
			if s := parseManifestField(m, "SizeOnDisk"); s != "" {
				size, _ = strconv.ParseInt(s, 10, 64)
			}
			cp := compatdataPath(o, id)
			_, prefixErr := os.Stat(cp)
			sh := shaderLibs(o, id)
			if name == "" {
				name = installdir
			}
			apps = append(apps, App{
				AppID: id, Name: name, InstallDir: installdir,
				Size: size, HumanSize: humanSize(size),
				Library: lib, ShortLib: shortLibName(o, lib),
				HasPrefix: prefixErr == nil, HasShader: len(sh) > 0,
				PrefixPath: cp,
			})
		}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps
}

// ---------- steam process ----------

func isSteamRunning(o Options) bool {
	if data, err := os.ReadFile(o.SteamPIDFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
				return true
			}
		}
	}
	return exec.Command("pgrep", "-x", "steam").Run() == nil
}

func stopSteamIfNeeded(o Options) error {
	if !isSteamRunning(o) {
		return nil
	}
	if o.DryRun {
		fmt.Fprintln(os.Stderr, "[warn] Steam is running — live run would close Steam, but dry-run continues.")
		return nil
	}
	if o.StopSteam || o.Force {
		fmt.Println("[steam-ctl] Shutting down Steam...")
		_ = exec.Command("steam", "-shutdown").Run()
		for i := 0; i < 15; i++ {
			if !isSteamRunning(o) {
				break
			}
			time.Sleep(time.Second)
		}
		if isSteamRunning(o) {
			fmt.Fprintln(os.Stderr, "[warn] Steam still running after 15s.")
			if !o.Force {
				return fmt.Errorf("steam still running")
			}
		} else {
			fmt.Println("[ok] Steam stopped.")
		}
		return nil
	}
	if isTTY() {
		if promptDefaultYes("Steam is running. Close Steam now?") {
			fmt.Println("[steam-ctl] Shutting down Steam...")
			_ = exec.Command("steam", "-shutdown").Run()
			for i := 0; i < 15; i++ {
				if !isSteamRunning(o) {
					break
				}
				time.Sleep(time.Second)
			}
			if isSteamRunning(o) {
				fmt.Fprintln(os.Stderr, "[warn] Steam still running after 15s.")
				if !o.Force {
					return fmt.Errorf("steam still running")
				}
			} else {
				fmt.Println("[ok] Steam stopped.")
			}
			return nil
		}
		return fmt.Errorf("aborted — steam still running (re-run with --stop-steam)")
	}
	return fmt.Errorf("steam is running — close Steam or use --stop-steam")
}

// ---------- prompts (non-TUI) ----------

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	fo, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fo.Mode()&os.ModeCharDevice != 0
}

func promptYesNo(msg string) bool {
	fmt.Printf("%s [y/N] ", msg)
	r := bufio.NewReader(os.Stdin)
	ans, _ := r.ReadString('\n')
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}

func promptDefaultYes(msg string) bool {
	fmt.Printf("%s [Y/n] ", msg)
	r := bufio.NewReader(os.Stdin)
	ans, _ := r.ReadString('\n')
	ans = strings.TrimSpace(ans)
	if ans == "" {
		return true
	}
	ans = strings.ToLower(ans)
	return ans == "y" || ans == "yes"
}

// ---------- operations ----------

func validAppID(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

func dirSize(p string) int64 {
	var n int64
	_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// MoveProgress is a live snapshot of the rsync copy phase.
type MoveProgress struct {
	Done     int64         // bytes transferred so far
	Total    int64         // total bytes (pre-scanned source size)
	SpeedBps float64       // current throughput, 0 if unknown
	ETA      time.Duration // 0 if unknown
}

func (p MoveProgress) Pct() float64 {
	if p.Total <= 0 {
		return 0
	}
	return float64(p.Done) / float64(p.Total) * 100
}

// rsync --info=progress2 emits lines like:
// "  123456789  45%    7.82MB/s    0:00:10 (xfr#123, to-chk=45/100)"
var rsyncProgRe = regexp.MustCompile(`^\s*([\d,]+)\s+(\d+)%\s+(\S+)/s\s+(\S+)`)

func parseSpeed(s string) float64 {
	mult := 1.0
	num := s
	switch {
	case strings.HasSuffix(s, "GB"):
		mult = 1024 * 1024 * 1024
		num = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult = 1024 * 1024
		num = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "kB"):
		mult = 1024
		num = strings.TrimSuffix(s, "kB")
	case strings.HasSuffix(s, "B"):
		num = strings.TrimSuffix(s, "B")
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(num, ",", ""), 64)
	if err != nil {
		return 0
	}
	return f * mult
}

func parseETA(s string) time.Duration {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var h, m, sec int
	if len(parts) == 3 {
		h, _ = strconv.Atoi(parts[0])
		m, _ = strconv.Atoi(parts[1])
		sec, _ = strconv.Atoi(parts[2])
	} else {
		m, _ = strconv.Atoi(parts[0])
		sec, _ = strconv.Atoi(parts[1])
	}
	if m < 0 || sec < 0 || h < 0 {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second
}

func parseRsyncProgress(line string) (done int64, pct float64, bps float64, eta time.Duration, ok bool) {
	m := rsyncProgRe.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, 0, 0, false
	}
	done, _ = strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	p, _ := strconv.ParseFloat(m[2], 64)
	return done, p, parseSpeed(m[3]), parseETA(m[4]), true
}

func cmdMove(o Options, appid, targetInput string) error {
	return moveWithContext(context.Background(), o, appid, targetInput, nil)
}

func moveWithContext(ctx context.Context, o Options, appid, targetInput string, prog func(MoveProgress)) error {
	if !validAppID(appid) {
		return fmt.Errorf("invalid appid: %s", appid)
	}
	target, err := resolveLibraryAlias(o, targetInput)
	if err != nil {
		target = targetInput
	}
	if ap, err := filepath.Abs(target); err == nil {
		target = ap
	}
	if st, err := os.Stat(filepath.Join(target, "steamapps")); err != nil || !st.IsDir() {
		return fmt.Errorf("target not a Steam library: %s → %s (expected %s/steamapps)", targetInput, target, target)
	}
	source, err := findSourceLib(o, appid)
	if err != nil {
		return err
	}
	if sp, err := filepath.EvalSymlinks(source); err == nil {
		source = sp
	}
	if tp, err := filepath.EvalSymlinks(target); err == nil {
		target = tp
	}
	if source == target {
		return fmt.Errorf("source and target are the same: %s", source)
	}
	manifest := filepath.Join(source, "steamapps", "appmanifest_"+appid+".acf")
	installdir := parseManifestField(manifest, "installdir")
	name := parseManifestField(manifest, "name")
	if name == "" {
		name = installdir
	}
	var size int64
	if s := parseManifestField(manifest, "SizeOnDisk"); s != "" {
		size, _ = strconv.ParseInt(s, 10, 64)
	}
	fmt.Printf("[steam-ctl] Move %s (%s) — %s (%s)\n", name, appid, installdir, humanSize(size))
	fmt.Printf("  Source: %s\n  Target: %s\n", filepath.Join(source, "steamapps", "common", installdir), filepath.Join(target, "steamapps", "common", installdir))
	cp := compatdataPath(o, appid)
	if _, err := os.Stat(cp); err == nil {
		fmt.Printf("  Prefix: PRESERVE %s (never deleted)\n", cp)
	} else {
		fmt.Println("  Prefix: none (will be created on first launch)")
	}
	srcCommon := filepath.Join(source, "steamapps", "common", installdir)
	dstCommon := filepath.Join(target, "steamapps", "common", installdir)
	srcManifest := manifest
	dstManifest := filepath.Join(target, "steamapps", "appmanifest_"+appid+".acf")

	needsOverwrite := false
	if _, err := os.Stat(dstCommon); err == nil {
		needsOverwrite = true
	}
	if _, err := os.Stat(dstManifest); err == nil {
		needsOverwrite = true
	}
	if needsOverwrite {
		fmt.Fprintf(os.Stderr, "[warn] Target already has %s at %s\n", filepath.Base(dstCommon), target)
		switch {
		case o.Force:
			fmt.Println("[steam-ctl] FORCE: will overwrite target")
		case o.DryRun:
		default:
			if isTTY() {
				if !promptDefaultYes("Overwrite target (just delete existing)?") {
					return fmt.Errorf("aborted — target exists, not overwriting")
				}
			} else {
				return fmt.Errorf("aborted — target exists, re-run with -f to overwrite")
			}
		}
	}
	if o.DryRun {
		if isSteamRunning(o) {
			fmt.Fprintln(os.Stderr, "[warn] Steam is running — live run would require close, but dry-run continues.")
		}
		if needsOverwrite {
			fmt.Printf("[dry-run] Would rm -rf %q and %q (overwrite)\n", dstCommon, dstManifest)
		}
		fmt.Printf("[dry-run] Would copy %q -> %q\n", srcCommon, dstCommon)
		fmt.Printf("[dry-run] Would mv %q -> %q\n", srcManifest, dstManifest)
		fmt.Printf("[dry-run] Would PRESERVE compatdata: %s\n", cp)
		return nil
	}
	if err := stopSteamIfNeeded(o); err != nil {
		return err
	}
	if needsOverwrite {
		fmt.Println("[steam-ctl] Removing existing target for overwrite...")
		_ = os.RemoveAll(dstCommon)
		_ = os.Remove(dstManifest)
	}
	if _, err := os.Stat(srcCommon); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] Source common missing: %s\n", srcCommon)
	}
	if !o.Force && isTTY() {
		if !promptYesNo("Proceed with move?") {
			fmt.Println("[steam-ctl] Aborted.")
			return nil
		}
	}
	_ = os.MkdirAll(filepath.Join(target, "steamapps", "common"), 0o755)
	if _, err := os.Stat(srcCommon); err == nil {
		if _, err := exec.LookPath("rsync"); err == nil {
			total := dirSize(srcCommon)
			args := []string{"-aH", "--info=progress2", srcCommon + "/", dstCommon + "/"}
			fmt.Printf("[steam-ctl] Copying %s -> %s (rsync, %s)...\n", installdir, target, humanSize(total))
			cmd := exec.CommandContext(ctx, "rsync", args...)
			stderr, err := cmd.StderrPipe()
			if err != nil {
				return fmt.Errorf("rsync pipe: %w", err)
			}
			if !o.Quiet {
				cmd.Stdout = os.Stdout
			}
			if err := cmd.Start(); err != nil {
				if ctx.Err() != nil {
					_ = os.RemoveAll(dstCommon)
					return fmt.Errorf("cancelled")
				}
				return fmt.Errorf("rsync start: %w", err)
			}
			// Parse progress2 lines off stderr. Always consumed (even when
			// Quiet) so rsync never blocks; forwarded to the prog callback
			// and echoed for CLI runs.
			doneCh := make(chan struct{})
			go func() {
				defer close(doneCh)
				sc := bufio.NewScanner(stderr)
				sc.Buffer(make([]byte, 64*1024), 64*1024)
				for sc.Scan() {
					line := sc.Text()
					if d, _, bps, eta, ok := parseRsyncProgress(line); ok {
						if prog != nil {
							prog(MoveProgress{Done: d, Total: total, SpeedBps: bps, ETA: eta})
						}
						if !o.Quiet {
							fmt.Fprintf(os.Stderr, "\r%-78s", line)
						}
					} else if !o.Quiet {
						_, _ = io.WriteString(os.Stderr, line+"\n")
					}
				}
			}()
			runErr := cmd.Wait()
			<-doneCh
			if !o.Quiet {
				fmt.Fprintln(os.Stderr)
			}
			if runErr != nil {
				if ctx.Err() != nil {
					_ = os.RemoveAll(dstCommon)
					return fmt.Errorf("cancelled — source left intact, partial target removed")
				}
				return fmt.Errorf("rsync failed: %w", runErr)
			}
			if prog != nil {
				prog(MoveProgress{Done: total, Total: total})
			}
			s1, s2 := dirSize(srcCommon), dirSize(dstCommon)
			if s1 != s2 {
				fmt.Fprintf(os.Stderr, "[warn] Size mismatch: source %d != dest %d\n", s1, s2)
			} else {
				fmt.Printf("[ok] Copy verified (%s)\n", humanSize(size))
			}
			fmt.Printf("[steam-ctl] Removing source: %s\n", srcCommon)
			_ = os.RemoveAll(srcCommon)
		} else {
			fmt.Println("[steam-ctl] rsync not found, using mv...")
			if err := os.Rename(srcCommon, dstCommon); err != nil {
				return fmt.Errorf("mv failed: %w", err)
			}
		}
	}
	fmt.Println("[steam-ctl] Moving manifest...")
	if err := os.Rename(srcManifest, dstManifest); err != nil {
		return fmt.Errorf("failed to move manifest: %w", err)
	}
	fmt.Printf("[ok] Move complete. Compatdata preserved: %s\n", cp)
	if sh := shaderLibs(o, appid); len(sh) > 0 {
		fmt.Printf("[ok] Shadercache preserved: %s\n", filepath.Join(sh[0], "steamapps", "shadercache", appid))
	}
	fmt.Println("[steam-ctl] Reopen Steam — game will appear in new library.")
	return nil
}

func cmdUninstall(o Options, appid string) error {
	if !validAppID(appid) {
		return fmt.Errorf("invalid appid: %s", appid)
	}
	source, err := findSourceLib(o, appid)
	if err != nil {
		return err
	}
	manifest := filepath.Join(source, "steamapps", "appmanifest_"+appid+".acf")
	installdir := parseManifestField(manifest, "installdir")
	name := parseManifestField(manifest, "name")
	if name == "" {
		name = installdir
	}
	var size int64
	if s := parseManifestField(manifest, "SizeOnDisk"); s != "" {
		size, _ = strconv.ParseInt(s, 10, 64)
	}
	fmt.Printf("[steam-ctl] Uninstall %s (%s) — %s (%s)\n", name, appid, installdir, humanSize(size))
	fmt.Printf("  Library: %s\n  To delete: %s\n  To delete: %s\n", source,
		filepath.Join(source, "steamapps", "common", installdir), manifest)
	cp := compatdataPath(o, appid)
	if _, err := os.Stat(cp); err == nil {
		if o.PurgeCompat {
			fmt.Printf("  Prefix: DELETE %s (--purge-compatdata)\n", cp)
		} else {
			fmt.Printf("  Prefix: PRESERVE %s (use --purge-compatdata to wipe)\n", cp)
		}
	} else {
		fmt.Println("  Prefix: none")
	}
	hasShader := len(shaderLibs(o, appid)) > 0
	if hasShader {
		if o.PurgeShader {
			fmt.Println("  Shadercache: DELETE (all libraries)")
		} else {
			fmt.Println("  Shadercache: PRESERVE (use --purge-shadercache to wipe)")
		}
	} else {
		fmt.Println("  Shadercache: none")
	}
	if o.DryRun {
		if isSteamRunning(o) {
			fmt.Fprintln(os.Stderr, "[warn] Steam is running — live run would require --stop-steam, but dry-run continues.")
		}
		fmt.Printf("[dry-run] Would rm -rf %q\n", filepath.Join(source, "steamapps", "common", installdir))
		fmt.Printf("[dry-run] Would rm %q\n", manifest)
		if o.PurgeCompat {
			fmt.Printf("[dry-run] Would rm -rf %q\n", cp)
		} else {
			fmt.Printf("[dry-run] Would PRESERVE %q\n", cp)
		}
		return nil
	}
	if err := stopSteamIfNeeded(o); err != nil {
		return err
	}
	if !o.Force && isTTY() {
		if !promptYesNo("Proceed with uninstall (game files deleted, prefix kept)?") {
			fmt.Println("[steam-ctl] Aborted.")
			return nil
		}
	}
	common := filepath.Join(source, "steamapps", "common", installdir)
	if _, err := os.Stat(common); err == nil {
		fmt.Printf("[steam-ctl] Deleting %s ...\n", common)
		_ = os.RemoveAll(common)
		fmt.Printf("[ok] Deleted common/%s\n", installdir)
	} else {
		fmt.Fprintf(os.Stderr, "[warn] Common not found: %s\n", common)
	}
	if _, err := os.Stat(manifest); err == nil {
		_ = os.Remove(manifest)
		fmt.Printf("[ok] Deleted appmanifest_%s.acf\n", appid)
	}
	if o.PurgeCompat {
		if _, err := os.Stat(cp); err == nil {
			fmt.Printf("[steam-ctl] Purging compatdata %s ...\n", cp)
			_ = os.RemoveAll(cp)
			fmt.Println("[ok] Purged compatdata")
		}
	} else if _, err := os.Stat(cp); err == nil {
		fmt.Printf("[ok] Preserved compatdata: %s — reinstall will reuse settings\n", cp)
	}
	if o.PurgeShader {
		for _, l := range getLibraries(o) {
			sp := filepath.Join(l, "steamapps", "shadercache", appid)
			if _, err := os.Stat(sp); err == nil {
				_ = os.RemoveAll(sp)
				fmt.Printf("[ok] Purged %s\n", sp)
			}
		}
	} else if hasShader {
		fmt.Println("[ok] Preserved shadercache")
	}
	fmt.Println("[ok] Uninstall complete. Reinstall via Steam UI later to reuse preserved prefix.")
	return nil
}

func cmdFixLibraries(o Options) error {
	fmt.Printf("[steam-ctl] Checking %s for duplicates...\n", o.LibraryVDF)
	data, err := os.ReadFile(o.LibraryVDF)
	if err != nil {
		return fmt.Errorf("no %s found", o.LibraryVDF)
	}
	fmt.Println(string(data))
	fmt.Println()
	contentRe := regexp.MustCompile(`"contentid"\s+"([^"]+)"`)
	counts := map[string]int{}
	for _, m := range contentRe.FindAllStringSubmatch(string(data), -1) {
		counts[m[1]]++
	}
	var dups []string
	for k, v := range counts {
		if v > 1 {
			dups = append(dups, k)
		}
	}
	if len(dups) == 0 {
		fmt.Println("[ok] No duplicate contentid found.")
	} else {
		fmt.Fprintf(os.Stderr, "[warn] Duplicate contentid(s): %s\n", strings.Join(dups, ", "))
		fmt.Fprintln(os.Stderr, "[warn] This indicates same drive mounted at two paths (e.g., /run/media vs /mnt).")
	}
	staleRe := regexp.MustCompile(`"path"\s+"([^"]*\/run\/media[^"]*)"`)
	stales := staleRe.FindAllStringSubmatch(string(data), -1)
	if len(stales) == 0 {
		fmt.Println("[ok] No stale /run/media entry — already clean.")
		return nil
	}
	fmt.Fprintln(os.Stderr, "[warn] Found stale entry(ies):")
	for _, s := range stales {
		fmt.Printf("  %s\n", s[1])
	}
	first := stales[0][1]
	if _, err := os.Stat(first); err == nil {
		fmt.Fprintf(os.Stderr, "[warn] But directory still exists (%s). Skipping auto-prune.\n", first)
		return nil
	}
	fmt.Println()
	if o.DryRun {
		fmt.Println("[dry-run] Would remove stale block(s) containing /run/media and reindex")
		fmt.Printf("[dry-run] Backup to: %s.bak.<timestamp>\n", o.LibraryVDF)
		return nil
	}
	proceed := o.Force
	if !proceed {
		if isTTY() {
			proceed = promptYesNo("Remove stale /run/media block(s) and reindex?")
		} else {
			return fmt.Errorf("re-run with -f to prune")
		}
	}
	if !proceed {
		fmt.Println("[steam-ctl] Aborted.")
		return nil
	}
	bak := fmt.Sprintf("%s.bak.%d", o.LibraryVDF, time.Now().Unix())
	orig, _ := os.ReadFile(o.LibraryVDF)
	_ = os.WriteFile(bak, orig, 0o644)
	fmt.Printf("[steam-ctl] Backup created: %s\n", bak)
	text := string(data)
	blockRe := regexp.MustCompile(`(?s)(\n\t)"(\d+)"\n\t\{(.*?)\n\t\}`)
	matches := blockRe.FindAllStringSubmatchIndex(text, -1)
	type blk struct{ body string }
	var kept []blk
	for _, m := range matches {
		body := text[m[6]:m[7]]
		full := text[m[0]:m[1]]
		_ = full
		if strings.Contains(body, "/run/media") {
			continue
		}
		kept = append(kept, blk{body})
	}
	if len(matches) > 0 {
		start, end := matches[0][0], matches[len(matches)-1][1]
		var nb strings.Builder
		for i, k := range kept {
			fmt.Fprintf(&nb, "\n\t\"%d\"\n\t{\n%s\n\t}", i, k.body)
		}
		_ = os.WriteFile(o.LibraryVDF, []byte(text[:start]+nb.String()+text[end:]), 0o644)
		fmt.Println("Reindexed libraries")
	}
	fmt.Printf("[ok] Pruned stale entry. Restore with: cp %s %s\n", bak, o.LibraryVDF)
	return nil
}
