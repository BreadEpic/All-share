package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exported config.js is a file a browser executes. Anything that could end
// the string, or the <script> element containing it, has to be escaped — and
// the service address is not a fixed value, it is whatever was typed during
// setup.
func TestJSStringEscaping(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary address", "wss://rv.example.com/rv", `"wss://rv.example.com/rv"`},
		{"double quote", `a"b`, `"a\"b"`},
		{"backslash", `a\b`, `"a\\b"`},
		{"newline", "a\nb", `"a\nb"`},
		{"carriage return", "a\rb", `"a\rb"`},
		{"tab", "a\tb", `"a\tb"`},
		// Escaped so a value containing "</script>" cannot end the element early.
		{"closing script tag", "</script>", `"\u003c/script>"`},
		// JavaScript treats these two as line terminators inside a string.
		{"line separator", "a\u2028b", `"a\u2028b"`},
		{"paragraph separator", "a\u2029b", `"a\u2029b"`},
		{"control character", "a\x01b", `"a\u0001b"`},
		{"empty", "", `""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jsString(c.in); got != c.want {
				t.Errorf("jsString(%q) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// A value that escapes its string literal would let whatever was typed as a
// service address run as code in the exported client.
func TestRenderConfigContainsNoBreakout(t *testing.T) {
	hostile := `wss://x/rv"; window.stolen = 1; //`
	out := renderConfig(hostile, `</script><script>alert(1)</script>`)

	if strings.Contains(out, `window.stolen`) && !strings.Contains(out, `\"`) {
		t.Error("the address escaped its string literal")
	}
	if strings.Contains(out, "</script>") {
		t.Error("a raw </script> reached the output and would end the element early")
	}
	// One statement, still assigning the config object.
	if !strings.Contains(out, "window.ALLSHARE_CONFIG = {") {
		t.Error("the config assignment is missing")
	}
}

// A zip entry names its own path, so an archive can ask to be written outside
// the folder it is being extracted into. Ours never would; an archive someone
// points -source at might.
func TestSafeJoinRefusesEscapes(t *testing.T) {
	root := t.TempDir()

	for _, bad := range []string{
		"../outside.txt",
		"../../outside.txt",
		"a/../../outside.txt",
		"/etc/passwd",
		"..",
	} {
		if got, err := safeJoin(root, bad); err == nil {
			t.Errorf("safeJoin(%q) allowed %q, want an error", bad, got)
		}
	}

	for _, good := range []string{"index.html", "js/app.js", "css/theme.css", "a/b/c.txt"} {
		got, err := safeJoin(root, good)
		if err != nil {
			t.Errorf("safeJoin(%q) failed: %v", good, err)
			continue
		}
		if !strings.HasPrefix(got, root) {
			t.Errorf("safeJoin(%q) = %q, which is outside %q", good, got, root)
		}
	}
}

// End to end through a real archive: a hostile entry must be refused rather
// than written, and the file it aimed at must not appear.
func TestCopyFromZipRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.zip")

	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, name := range []string{"client/index.html", "client/../../escaped.txt"} {
		e, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if _, err := copyFromZip(archive, dst); err == nil {
		t.Fatal("an entry pointing outside the folder was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); err == nil {
		t.Fatal("a file was written outside the target folder")
	}
}

// The packaged zip wraps the client in one folder. Stripping it is what makes
// the output openable directly rather than a folder containing a folder.
func TestCommonZipPrefix(t *testing.T) {
	mk := func(names ...string) []*zip.File {
		out := make([]*zip.File, 0, len(names))
		for _, n := range names {
			out = append(out, &zip.File{FileHeader: zip.FileHeader{Name: n}})
		}
		return out
	}
	if got := commonZipPrefix(mk("client/index.html", "client/js/app.js")); got != "client/" {
		t.Errorf("shared wrapper: got %q, want %q", got, "client/")
	}
	if got := commonZipPrefix(mk("index.html", "js/app.js")); got != "" {
		t.Errorf("no wrapper: got %q, want empty", got)
	}
	if got := commonZipPrefix(mk("a/index.html", "b/app.js")); got != "" {
		t.Errorf("two different top folders: got %q, want empty", got)
	}
}

// The whole point of the command: the exported copy must carry the address, so
// nothing has to be typed on the other device.
func TestExportClientWritesTheAddress(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "client")
	if err := os.MkdirAll(filepath.Join(src, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"index.html": "<html></html>",
		"config.js":  "window.ALLSHARE_CONFIG = { rendezvous: \"\" };",
		"js/app.js":  "// app",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(dir, "out")
	const service = "wss://rv.example.com/rv"
	if _, err := exportClient(src, dst, service, "Chromebook"); err != nil {
		t.Fatalf("export: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "config.js"))
	if err != nil {
		t.Fatalf("read exported config: %v", err)
	}
	if !strings.Contains(string(got), service) {
		t.Errorf("the exported config.js does not carry the service address:\n%s", got)
	}
	if !strings.Contains(string(got), "Chromebook") {
		t.Errorf("the exported config.js does not carry the device label:\n%s", got)
	}
	// The rest of the client must come across too, or there is nothing to open.
	for _, name := range []string{"index.html", "js/app.js"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("%s was not exported: %v", name, err)
		}
	}
}
