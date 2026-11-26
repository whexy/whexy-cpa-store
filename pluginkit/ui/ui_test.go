package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestPagePlacesTheKitAheadOfThePage(t *testing.T) {
	got := string(Page([]byte("<head><style>.own{}</style></head><body><script>own()</script></body>")))
	kitStyle, ownStyle := strings.Index(got, "--bg-page"), strings.Index(got, ".own{}")
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
