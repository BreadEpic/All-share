package main

import (
	"archive/zip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mmc/all-share/internal/config"
)

// cmdClient writes out a copy of the browser client that already knows this
// PC's service address.
//
// Without this, setting up a new device means typing a wss:// URL into a phone
// or a Chromebook by hand — the single worst step in the whole product, and one
// the user has already completed once on this PC. The address is written into
// config.js, which the client reads on startup, so the folder that comes out of
// here can be copied to another device and opened. Nothing to type, nothing to
// configure.
func cmdClient(args []string) error {
	flags := flag.NewFlagSet("client", flag.ExitOnError)
	dataDir := flags.String("data", "", "directory for settings and identity")
	out := flags.String("out", "", "folder to write the client into")
	source := flags.String("source", "", "client zip or folder to copy from (found automatically by default)")
	label := flags.String("label", "", "name to suggest for the new device")
	if err := flags.Parse(args); err != nil {
		return err
	}

	dir := *dataDir
	if dir == "" {
		resolved, err := config.Dir()
		if err != nil {
			return err
		}
		dir = resolved
	}
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	if cfg.Rendezvous == "" {
		return errors.New("this PC has no ALL SHARE service address yet. " +
			"Set one first: allshare-agent config -service wss://your-service/rv")
	}

	target := *out
	if target == "" {
		target = defaultClientOutput()
	}

	src := *source
	if src == "" {
		src, err = findClientSource()
		if err != nil {
			return err
		}
	}

	written, err := exportClient(src, target, cfg.Rendezvous, *label)
	if err != nil {
		return err
	}

	fmt.Printf("Wrote the ALL SHARE client to:\n\n    %s\n\n", target)
	fmt.Printf("It already knows this PC's service address, so there is nothing to type.\n")
	fmt.Printf("Copy that whole folder to your other device and open index.html.\n\n")
	fmt.Printf("(%d files)\n", written)
	return nil
}

// defaultClientOutput picks somewhere the user will actually find.
func defaultClientOutput() string {
	if home, err := os.UserHomeDir(); err == nil {
		desktop := filepath.Join(home, "Desktop")
		if info, err := os.Stat(desktop); err == nil && info.IsDir() {
			return filepath.Join(desktop, "ALL SHARE Client")
		}
		return filepath.Join(home, "ALL SHARE Client")
	}
	return "ALL SHARE Client"
}

// findClientSource locates the client files the installer shipped.
//
// The installed layout puts allshare-client.zip beside the executable. A
// developer running from a checkout has client/ instead. Both are worth
// supporting, because being unable to find them is a confusing failure and the
// two locations are cheap to check.
func findClientSource() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	base := filepath.Dir(exe)

	candidates := []string{
		filepath.Join(base, "allshare-client.zip"),
		filepath.Join(base, "client"),
		filepath.Join(base, "..", "client"),
		filepath.Join(base, "..", "..", "client"),
		filepath.Join(base, "..", "..", "..", "client"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil {
			if info.IsDir() {
				if _, err := os.Stat(filepath.Join(c, "index.html")); err != nil {
					continue
				}
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("could not find the ALL SHARE client files near %s. "+
		"Point at them with -source", base)
}

// exportClient copies the client to dst and rewrites its configuration.
func exportClient(src, dst, service, label string) (int, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}

	var (
		count int
		err   error
	)
	if strings.EqualFold(filepath.Ext(src), ".zip") {
		count, err = copyFromZip(src, dst)
	} else {
		count, err = copyFromDir(src, dst)
	}
	if err != nil {
		return 0, err
	}

	if err := os.WriteFile(filepath.Join(dst, "config.js"),
		[]byte(renderConfig(service, label)), 0o644); err != nil {
		return 0, fmt.Errorf("write config.js: %w", err)
	}
	return count, nil
}

// safeJoin resolves name against root, refusing anything that escapes it.
//
// A zip entry controls its own path, so "../../etc/passwd" is a thing an
// archive can contain. This is our own archive today, but a path check that
// only exists when the input is trusted is a check that disappears the moment
// somebody points -source at a download.
func safeJoin(root, name string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing an entry that points outside the folder: %q", name)
	}
	joined := filepath.Join(root, cleaned)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing an entry that points outside the folder: %q", name)
	}
	return joined, nil
}

func copyFromZip(src, dst string) (int, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", src, err)
	}
	defer r.Close()

	// The packaged zip wraps everything in one top-level folder. Strip it so the
	// output is the client itself rather than a folder containing a folder.
	prefix := commonZipPrefix(r.File)

	count := 0
	for _, f := range r.File {
		name := strings.TrimPrefix(f.Name, prefix)
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		path, err := safeJoin(dst, name)
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return 0, err
		}
		rc, err := f.Open()
		if err != nil {
			return 0, err
		}
		out, err := os.Create(path)
		if err != nil {
			rc.Close()
			return 0, err
		}
		// Bounded: a client file is a few tens of kilobytes, and an unbounded
		// copy from an archive is how a zip bomb fills a disk.
		if _, err := io.Copy(out, io.LimitReader(rc, 8<<20)); err != nil {
			rc.Close()
			out.Close()
			return 0, err
		}
		rc.Close()
		if err := out.Close(); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

// commonZipPrefix returns the single top-level folder every entry shares, or "".
func commonZipPrefix(files []*zip.File) string {
	prefix := ""
	for _, f := range files {
		slash := strings.Index(f.Name, "/")
		if slash < 0 {
			return "" // something lives at the root, so there is no wrapper
		}
		top := f.Name[:slash+1]
		if prefix == "" {
			prefix = top
		} else if prefix != top {
			return ""
		}
	}
	return prefix
}

func copyFromDir(src, dst string) (int, error) {
	count := 0
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target, err := safeJoin(dst, rel)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil // skip symlinks and anything else unusual
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// renderConfig produces the client's config.js with the service address filled
// in. Both values are JSON-quoted, so a quote or backslash in either cannot
// break out of the string and corrupt the file.
func renderConfig(service, label string) string {
	return fmt.Sprintf(`/*
 * ALL SHARE — local configuration
 *
 * Written by "allshare-agent client" so this copy already points at the right
 * service. There is nothing to fill in: open index.html and pair.
 *
 * Anything changed in the app's Settings screen wins over what is here.
 */
window.ALLSHARE_CONFIG = {
  rendezvous: %s,
  deviceLabel: %s
};
`, jsString(service), jsString(label))
}

// jsString quotes a value as a JavaScript string literal.
//
// The result is written into a file that a browser executes, so every character
// that could end the string or the surrounding <script> element is escaped:
// quotes and backslashes for the obvious reason, "<" so that a value containing
// "</script>" cannot break out of the element, and U+2028/U+2029 because
// JavaScript treats those as line terminators inside a string literal.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '<':
			b.WriteString(`\u003c`)
		case '\u2028':
			b.WriteString(`\u2028`)
		case '\u2029':
			b.WriteString(`\u2029`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
