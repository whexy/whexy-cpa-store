package manage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// PageCSP admits only the page's own inline scripts and styles and requests
// to its own origin, so injected markup can neither run nor send the
// management key elsewhere. It sets no frame-ancestors because Management
// Center may be served from another origin than CLIProxyAPI.
func PageCSP(page []byte) string {
	return "default-src 'none'; script-src " + inlineHashes(page, "script") +
		"; style-src " + inlineHashes(page, "style") +
		"; connect-src 'self'; base-uri 'none'; form-action 'none'"
}

// inlineHashes lists the CSP hash source of every bare <tag> block in page.
func inlineHashes(page []byte, tag string) string {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	var hashes []string
	for {
		start := bytes.Index(page, open)
		if start < 0 {
			break
		}
		page = page[start+len(open):]
		stop := bytes.Index(page, end)
		if stop < 0 {
			break
		}
		sum := sha256.Sum256(page[:stop])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		page = page[stop+len(end):]
	}
	if len(hashes) == 0 {
		return "'none'"
	}
	return strings.Join(hashes, " ")
}
