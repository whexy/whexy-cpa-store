// Package ui holds the stylesheet and script every store page shares, so
// the pages look and behave alike inside Management Center.
//
// Pages are served under a CSP that admits only hashed inline blocks, so the
// kit is inlined rather than linked: Page places kit.css ahead of the page's
// first <style> and kit.js ahead of its first <script>. A page's source keeps
// only its own rules and logic, and its logic tests can still read the
// page's first <script> as its own.
package ui

import (
	"bytes"
	_ "embed"
)

// CSS is the shared stylesheet; see kit.css for its tokens and components.
//
//go:embed kit.css
var CSS []byte

// JS is the shared runtime, which defines the global `kit`.
//
//go:embed kit.js
var JS []byte

// Page returns html with the kit inlined. A page without <style> gets the
// stylesheet before </head>; one without <script> gets the runtime before
// </body>.
func Page(html []byte) []byte {
	out := insertBefore(html, [][]byte{[]byte("<style>"), []byte("</head>")}, block("style", CSS))
	return insertBefore(out, [][]byte{[]byte("<script>"), []byte("</body>")}, block("script", JS))
}

func block(tag string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString("<" + tag + ">\n")
	b.Write(body)
	b.WriteString("</" + tag + ">\n    ")
	return b.Bytes()
}

// insertBefore inserts content before the first marker found, trying markers
// in order, or appends it when none occurs.
func insertBefore(html []byte, markers [][]byte, content []byte) []byte {
	at := len(html)
	for _, marker := range markers {
		if i := bytes.Index(html, marker); i >= 0 {
			at = i
			break
		}
	}
	out := make([]byte, 0, len(html)+len(content))
	out = append(out, html[:at]...)
	out = append(out, content...)
	return append(out, html[at:]...)
}
