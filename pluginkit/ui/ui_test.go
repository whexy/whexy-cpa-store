package ui

import (
	"bytes"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestPagePlacesTheKitAheadOfThePage(t *testing.T) {
	got := string(Page([]byte("<head><style>.own{}</style></head><body><script>own()</script></body>")))
	kitStyle, ownStyle := strings.Index(got, "--color-canvas"), strings.Index(got, ".own{}")
	kitScript, ownScript := strings.Index(got, "globalThis.kit"), strings.Index(got, "own()")
	if kitStyle < 0 || kitStyle > ownStyle || kitScript < 0 || kitScript > ownScript {
		t.Errorf("kit not ahead of the page:\n%s", got)
	}
}

func TestPageWithoutBlocks(t *testing.T) {
	got := Page([]byte("<head></head><body></body>"))
	if !bytes.Contains(got, []byte("</style>\n    </head>")) || !bytes.Contains(got, []byte("</script>\n    </body>")) {
		t.Errorf("kit not placed before </head> and </body>:\n%s", got)
	}
}

func TestKitCannotCloseItsBlock(t *testing.T) {
	if bytes.Contains(CSS, []byte("</style")) || bytes.Contains(JS, []byte("</script")) {
		t.Error("kit source closes its own inline block")
	}
}

func TestEveryFontTheStylesheetNamesIsEmbedded(t *testing.T) {
	names := regexp.MustCompile(`url\("?(fonts/[^")]+)"?\)`).FindAllSubmatch(CSS, -1)
	if len(names) == 0 {
		t.Fatal("kit.css names no fonts")
	}
	for _, name := range names {
		if _, errStat := fs.Stat(Fonts, string(name[1])); errStat != nil {
			t.Errorf("kit.css names %s: %v", name[1], errStat)
		}
	}
}
