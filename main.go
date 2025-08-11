// packprompt: pack a tree of text files into one prompt-safe text blob, and unpack it back.
//
// Usage:
//
//	Pack:   ./packprompt pack --root . --out files-prompt.txt --exclude ".git,.idea,node_modules,*.png"
//	Unpack: ./packprompt unpack --in files-prompt.txt --dest ./recreated
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const (
	startMark = "--- FILE"
	endMark   = "--- END FILE ---"
)

var defaultExcludes = []string{
	".git", ".svn", ".hg", ".idea", ".vscode", "node_modules", ".venv", ".DS_Store",
	"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.ico",
	"*.pdf", "*.zip", "*.tar", "*.gz", "*.xz", "*.7z", "*.rar", "*.jar", "*.war",
	"*.class", "*.so", "*.dll", "*.dylib", "*.bin", "*.exe",
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "pack":
		packCmd(os.Args[2:])
	case "unpack":
		unpackCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`packprompt

Commands:
  pack   [--root DIR] [--out FILE] [--exclude PAT1,PAT2,...]
  unpack [--in FILE]  [--dest DIR]

Details:
  - Skips binary files and all non-regular files (FIFOs, sockets, devices, symlinks).
  - Default excludes: ` + strings.Join(defaultExcludes, ",") + `
  - Stores file mode and restores on unpack.
`)
}

func packCmd(args []string) {
	flg := flag.NewFlagSet("pack", flag.ExitOnError)
	root := flg.String("root", ".", "root directory to walk")
	out := flg.String("out", "files-prompt.txt", "output prompt file")
	excl := flg.String("exclude", strings.Join(defaultExcludes, ","), "comma-separated glob patterns to exclude")
	_ = flg.Parse(args)

	excludes := parseExcludes(*excl)
	outf, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer outf.Close()
	w := bufio.NewWriter(outf)
	defer w.Flush()

	err = filepath.WalkDir(*root, func(p string, d iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(*root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		// Exclusions first
		if excluded(rel, d, excludes) {
			if d.IsDir() {
				return iofs.SkipDir
			}
			return nil
		}

		// Only process regular files; skip dirs, symlinks, sockets, devices, FIFOs, etc.
		if !d.Type().IsRegular() {
			return nil
		}

		// Binary check (only on regular files)
		bin, err := isBinaryFile(p)
		if err != nil {
			// unreadable -> skip quietly
			return nil
		}
		if bin {
			return nil
		}

		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()

		info, err := d.Info()
		if err != nil {
			return nil
		}
		mode := info.Mode().Perm()

		if _, err := fmt.Fprintf(w, "%s path=%s mode=%04o ---\n", startMark, rel, mode); err != nil {
			return err
		}

		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"+endMark+"\n"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("Packed to %s\n", *out)
}

func unpackCmd(args []string) {
	flg := flag.NewFlagSet("unpack", flag.ExitOnError)
	in := flg.String("in", "files-prompt.txt", "input prompt file")
	dest := flg.String("dest", ".", "destination directory to unpack into")
	_ = flg.Parse(args)

	if err := os.MkdirAll(*dest, 0o755); err != nil {
		fatal(err)
	}
	f, err := os.Open(*in)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	r := bufio.NewReader(f)

	headerRe := regexp.MustCompile(`^--- FILE path=([^[:space:]]+)\ mode=([0-7]{3,4}) ---$`)

	for {
		line, err := readLine(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			fatal(err)
		}
		if !strings.HasPrefix(line, startMark) {
			continue
		}
		m := headerRe.FindStringSubmatch(line)
		if m == nil {
			fatal(fmt.Errorf("malformed header: %q", line))
		}
		rel := m[1]
		modeStr := m[2]
		if strings.Contains(rel, "..") && !safeRel(rel) {
			fatal(fmt.Errorf("unsafe path in archive: %q", rel))
		}
		full := filepath.Join(*dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fatal(err)
		}

		var contentBuf bytes.Buffer
		for {
			l, err := readLine(r)
			if err != nil {
				fatal(err)
			}
			if l == endMark {
				break
			}
			contentBuf.WriteString(l)
			contentBuf.WriteString("\n")
		}

		tmp := full + ".tmp~ftp"
		outf, err := os.Create(tmp)
		if err != nil {
			fatal(err)
		}
		contentBytes := contentBuf.Bytes()
		// Remove trailing newline if present
		if len(contentBytes) > 0 && contentBytes[len(contentBytes)-1] == '\n' {
			contentBytes = contentBytes[:len(contentBytes)-1]
		}
		if _, err := outf.Write(contentBytes); err != nil {
			_ = outf.Close()
			_ = os.Remove(tmp)
			fatal(err)
		}
		if err := outf.Close(); err != nil {
			_ = os.Remove(tmp)
			fatal(err)
		}

		var mode iofs.FileMode = 0o644
		if m, perr := parseOctal(modeStr); perr == nil {
			mode = m
		}
		_ = os.Chmod(tmp, mode)
		if err := os.Rename(tmp, full); err != nil {
			fatal(err)
		}
	}
	fmt.Printf("Unpacked into %s\n", *dest)
}

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if errors.Is(err, io.EOF) && len(s) > 0 {
		return strings.TrimRight(s, "\r\n"), nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func parseExcludes(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, filepath.ToSlash(p))
		}
	}
	return out
}

// patterns with '/' match whole relative path; otherwise match basename
func excluded(rel string, d iofs.DirEntry, patterns []string) bool {
	base := path.Base(rel)
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if strings.Contains(pat, "/") {
			if ok, _ := path.Match(pat, rel); ok {
				return true
			}
		} else {
			if ok, _ := path.Match(pat, base); ok {
				return true
			}
			if !strings.ContainsAny(pat, "*?[]") && base == pat {
				return true
			}
		}
	}
	return false
}

func safeRel(rel string) bool {
	clean := path.Clean("/" + rel)
	return !strings.HasPrefix(clean, "/../") && clean != "/.."
}

// Only called for regular files now; read a small sniff to classify
func isBinaryFile(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()

	const sniff = 8192
	buf := make([]byte, sniff)
	n, _ := io.ReadAtLeast(f, buf, 1) // read at least 1 byte; don't block for full 8K
	buf = buf[:n]
	if n == 0 {
		return false, nil
	}

	// Heuristic 1: NUL byte
	if bytes.IndexByte(buf, 0x00) >= 0 {
		return true, nil
	}

	// Heuristic 2: MIME
	ct := http.DetectContentType(buf)
	if strings.Contains(ct, "application/octet-stream") ||
		strings.Contains(ct, "application/x-executable") ||
		strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "font/") {
		return true, nil
	}

	// Heuristic 3: printable ratio
	var nonPrintable, printable int
	for _, r := range string(buf) {
		if r == '\n' || r == '\r' || r == '\t' {
			printable++
			continue
		}
		if r == '\uFFFD' {
			nonPrintable++
			continue
		}
		if r < 32 || (!unicode.IsPrint(r) && !unicode.IsSpace(r)) {
			nonPrintable++
		} else {
			printable++
		}
	}
	if printable == 0 {
		return true, nil
	}
	if float64(nonPrintable)/float64(printable+nonPrintable) > 0.30 {
		return true, nil
	}
	return false, nil
}

func parseOctal(s string) (iofs.FileMode, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	var v uint32
	for _, ch := range s {
		if ch < '0' || ch > '7' {
			return 0, fmt.Errorf("invalid octal %q", s)
		}
		v = (v << 3) | uint32(ch-'0')
	}
	return iofs.FileMode(v), nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
