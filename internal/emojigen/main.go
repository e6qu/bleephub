// Command emojigen builds bleephub/emoji_assets.tar.gz, the embedded archive
// backing GET /images/icons/emoji/{path}.
//
// Unicode emoji rasters come from the Twemoji project (the maintained
// jdecked/twemoji fork), whose 72×72 PNG graphics are licensed CC-BY 4.0. The
// tool maps every `unicode/<codepoints>.png` path in gemoji_catalog.txt to its
// Twemoji 72×72 filename: gemoji zero-pads codepoints to four hex digits and
// omits U+FE0F (variation selector-16) and U+200D (zero-width joiner) from
// ZWJ-sequence names, while Twemoji uses minimal hex and keeps those codepoints,
// so both sides are normalized (strip leading zeros, drop fe0f/200d tokens)
// before matching. The run fails if any catalog path does not resolve or the
// normalization is ambiguous.
//
// GitHub-custom emoji (catalog paths with no unicode/ prefix — octocat,
// shipit, trollface, …) are original bleephub artwork generated
// deterministically by the emojiart package.
//
// The archive is byte-reproducible: entries are sorted, timestamps zeroed,
// and ownership fields cleared.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/emojiart"
)

// The unicode assets come from one immutable jdecked/twemoji commit — the one
// release tag v17.0.3 pointed at when it was vendored — and the tarball's SHA-256
// is checked before anything reads it. A tag can be moved; a commit cannot, and
// the digest catches a rewritten archive either way. Bumping the pin means
// changing all three constants together and regenerating.
const (
	twemojiVersion       = "17.0.3"
	twemojiCommit        = "b6b55fef1e8636b540a6d016a4729ca8cdf2e60b"
	twemojiTarballSHA256 = "705d79de1460e5e775f362f0d0f01fbe3ef8d65bf4648c490e4649704584f747"
)

const twemojiTarballURL = "https://codeload.github.com/jdecked/twemoji/tar.gz/" + twemojiCommit

// twemojiHTTPTimeout bounds the whole download; the archive is a few megabytes.
const twemojiHTTPTimeout = 5 * time.Minute

func main() {
	catalogPath := flag.String("catalog", "gemoji_catalog.txt", "path to the gemoji name→path catalog")
	outPath := flag.String("out", "emoji_assets.tar.gz", "output archive path")
	tarballPath := flag.String("twemoji-tarball", "", "local copy of the pinned twemoji source tarball; downloaded from GitHub when empty. Must match twemojiTarballSHA256 either way")
	flag.Parse()

	catalog, err := os.ReadFile(localBuildPath(*catalogPath))
	if err != nil {
		log.Fatalf("read catalog: %v", err)
	}

	tarball, err := readTwemojiTarball(*tarballPath)
	if err != nil {
		log.Fatalf("twemoji tarball: %v", err)
	}
	rasters, license, err := extractTwemoji(tarball)
	if err != nil {
		log.Fatalf("extract twemoji assets: %v", err)
	}
	log.Printf("twemoji v%s (%s), sha256 %s: %d 72x72 rasters",
		twemojiVersion, twemojiCommit, twemojiTarballSHA256, len(rasters))

	normalized, err := normalizedIndex(rasters)
	if err != nil {
		log.Fatalf("index twemoji filenames: %v", err)
	}

	entries := map[string][]byte{
		"LICENSE-TWEMOJI": append(fmt.Appendf(nil,
			"The PNG images under unicode/ are Twemoji graphics\n"+
				"(https://github.com/jdecked/twemoji, release v%s),\n"+
				"copyright 2019 Twitter, Inc and other contributors,\n"+
				"copyright 2023 Twemoji contributors, licensed under\n"+
				"Creative Commons Attribution 4.0 International (CC-BY 4.0):\n\n", twemojiVersion), license...),
		"ATTRIBUTION": fmt.Appendf(nil,
			"unicode/*.png — Twemoji v%s graphics (CC-BY 4.0, see LICENSE-TWEMOJI),\n"+
				"filenames rewritten to the gemoji catalog's codepoint naming.\n\n"+
				"All other *.png — original bleephub artwork generated deterministically\n"+
				"by bleephub/internal/emojiart (same license as the bleephub source tree).\n", twemojiVersion),
	}

	var unicodeCount, customCount int
	var unresolved []string
	for _, line := range strings.Split(strings.TrimSpace(string(catalog)), "\n") {
		name, rawPath, found := strings.Cut(line, " ")
		if !found {
			log.Fatalf("malformed catalog line: %q", line)
		}
		assetPath := strings.TrimSuffix(rawPath, "?v8")
		if _, dup := entries[assetPath]; dup {
			continue // several names (e.g. +1 / thumbsup) share one image
		}
		if cp, isUnicode := strings.CutPrefix(assetPath, "unicode/"); isUnicode {
			cp = strings.TrimSuffix(cp, ".png")
			raster, ok := rasters[cp]
			if !ok {
				raster, ok = rasters[normalized[normalizeCodepoints(cp)]]
			}
			if !ok {
				unresolved = append(unresolved, assetPath)
				continue
			}
			entries[assetPath] = raster
			unicodeCount++
			continue
		}
		badge, err := emojiart.BadgePNG(name)
		if err != nil {
			log.Fatalf("generate custom emoji %q: %v", name, err)
		}
		entries[assetPath] = badge
		customCount++
	}
	if len(unresolved) > 0 {
		log.Fatalf("%d catalog unicode path(s) have no twemoji raster:\n  %s",
			len(unresolved), strings.Join(unresolved, "\n  "))
	}

	archive, err := buildArchive(entries)
	if err != nil {
		log.Fatalf("build archive: %v", err)
	}
	// #nosec G306 -- generated emoji artwork is a distributable public asset.
	if err := os.WriteFile(localBuildPath(*outPath), archive, 0o644); err != nil {
		log.Fatalf("write %s: %v", *outPath, err)
	}
	log.Printf("wrote %s: %d unicode + %d custom emoji, %d bytes",
		*outPath, unicodeCount, customCount, len(archive))
}

// readTwemojiTarball returns the pinned twemoji source tarball, from disk when
// -twemoji-tarball names one and from the pinned commit otherwise. Either way the
// bytes are rejected unless they hash to twemojiTarballSHA256, before any caller
// extracts them.
func readTwemojiTarball(localPath string) ([]byte, error) {
	tarball, err := fetchTwemojiTarball(localPath)
	if err != nil {
		return nil, err
	}
	got := fmt.Sprintf("%x", sha256.Sum256(tarball))
	if got != twemojiTarballSHA256 {
		source := twemojiTarballURL
		if localPath != "" {
			source = localPath
		}
		return nil, fmt.Errorf("%s hashes to %s, want %s for twemoji commit %s: refusing to vendor unverified bytes",
			source, got, twemojiTarballSHA256, twemojiCommit)
	}
	return tarball, nil
}

// buildPathPattern restricts operator-supplied build paths to an explicit safe
// alphabet; combined with the "no .." check it forbids parent-directory
// traversal in the catalog, output, and tarball paths.
var buildPathPattern = regexp.MustCompile(`^/?[A-Za-z0-9._/-]+$`)

// localBuildPath validates and canonicalizes an operator-supplied build path.
// emojigen is a build-time command; its paths come from the operator's command
// line, never a request. A path outside the safe alphabet or containing ".." is
// rejected; the rest resolve to a clean absolute path before any file is opened.
func localBuildPath(p string) string {
	if p == "" || strings.Contains(p, "..") || !buildPathPattern.MatchString(p) {
		log.Fatalf("path %q is not an allowed build path", p)
	}
	resolved, err := filepath.Abs(p)
	if err != nil {
		log.Fatalf("resolve path %q: %v", p, err)
	}
	return filepath.Clean(resolved)
}

func fetchTwemojiTarball(localPath string) ([]byte, error) {
	if localPath != "" {
		// #nosec G304 -- localPath is explicitly selected by the build operator.
		return os.ReadFile(localBuildPath(localPath))
	}
	return fetchTwemojiFromUpstream()
}

// fetchTwemojiFromUpstream downloads the pinned twemoji source tarball. It takes
// no caller input: the request target is derived solely from the compile-time
// constant twemojiTarballURL, and is validated https against the single expected
// host before the request, so it can never be pointed at an internal or
// attacker-chosen address. The response bytes are still SHA-256 pinned by
// readTwemojiTarball before anything extracts them.
func fetchTwemojiFromUpstream() ([]byte, error) {
	if err := ensureTwemojiURLIsPinnedHTTPS(); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: twemojiHTTPTimeout}
	// The request target is the compile-time constant validated just above; it
	// is never derived from caller input, an environment value, or a redirect.
	// deepcode ignore Ssrf: twemojiTarballURL is a compile-time constant, validated https/host by ensureTwemojiURLIsPinnedHTTPS and SHA-256 pinned after fetch. This is a build-time vendoring tool; the target is never attacker- or input-controlled.
	resp, err := client.Get(twemojiTarballURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", twemojiTarballURL, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// ensureTwemojiURLIsPinnedHTTPS rejects the vendored upstream URL unless it is
// https pointed at the single expected host. twemojiTarballURL is a constant, so
// this always passes at runtime; it makes the fetch target provably fixed and
// fails loudly if the constant is ever edited to something unsafe.
func ensureTwemojiURLIsPinnedHTTPS() error {
	target, err := url.Parse(twemojiTarballURL)
	if err != nil || target.Scheme != "https" || target.Hostname() != "codeload.github.com" {
		return fmt.Errorf("refusing to fetch twemoji from non-https or unexpected host: %q", twemojiTarballURL)
	}
	return nil
}

// extractTwemoji pulls the 72×72 PNG rasters (keyed by codepoint filename
// without extension) and the CC-BY 4.0 graphics license text out of the
// twemoji source tarball.
func extractTwemoji(tarball []byte) (rasters map[string][]byte, license []byte, err error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = gz.Close() }()
	rasters = make(map[string][]byte, 4096)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := path.Base(hdr.Name)
		switch {
		case strings.Contains(hdr.Name, "/assets/72x72/") && strings.HasSuffix(base, ".png"):
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, nil, fmt.Errorf("read %s: %w", hdr.Name, err)
			}
			rasters[strings.TrimSuffix(base, ".png")] = data
		case base == "LICENSE-GRAPHICS":
			license, err = io.ReadAll(tr)
			if err != nil {
				return nil, nil, fmt.Errorf("read %s: %w", hdr.Name, err)
			}
		}
	}
	if len(rasters) == 0 {
		return nil, nil, fmt.Errorf("no assets/72x72/*.png entries in tarball")
	}
	if len(license) == 0 {
		return nil, nil, fmt.Errorf("no LICENSE-GRAPHICS entry in tarball")
	}
	return rasters, license, nil
}

// normalizedIndex maps normalizeCodepoints(twemoji filename) → twemoji
// filename and errors if two filenames normalize to the same key, which
// would make gemoji→twemoji resolution ambiguous.
func normalizedIndex(rasters map[string][]byte) (map[string]string, error) {
	idx := make(map[string]string, len(rasters))
	for cp := range rasters {
		key := normalizeCodepoints(cp)
		if prev, dup := idx[key]; dup {
			return nil, fmt.Errorf("ambiguous normalization %q: %q vs %q", key, prev, cp)
		}
		idx[key] = cp
	}
	return idx, nil
}

// normalizeCodepoints strips leading zeros from each hyphen-separated hex
// codepoint and drops fe0f (variation selector-16) and 200d (zero-width
// joiner) tokens, reconciling gemoji's and Twemoji's filename conventions.
func normalizeCodepoints(cp string) string {
	var out []string
	for _, tok := range strings.Split(cp, "-") {
		tok = strings.TrimLeft(tok, "0")
		if tok == "" {
			tok = "0"
		}
		if tok == "fe0f" || tok == "200d" {
			continue
		}
		out = append(out, tok)
	}
	return strings.Join(out, "-")
}

// buildArchive writes the entries as a byte-reproducible tar.gz: sorted
// names, zeroed timestamps, no ownership metadata.
func buildArchive(entries map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data := entries[name]
		if err := tw.WriteHeader(&tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: time.Unix(0, 0).UTC(),
			Format:  tar.FormatPAX,
		}); err != nil {
			return nil, fmt.Errorf("header %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
