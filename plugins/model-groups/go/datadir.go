package main

/*
#cgo linux LDFLAGS: -ldl
#define _GNU_SOURCE
#include <dlfcn.h>

static const char* model_groups_library_path(void) {
	Dl_info info;
	if (dladdr((void*)&model_groups_library_path, &info) == 0 || info.dli_fname == NULL) {
		return NULL;
	}
	return info.dli_fname;
}
*/
import "C"

import (
	"path/filepath"
	"strings"
)

// defaultDataDir places the session bindings beside the plugins directory this
// library was loaded from. A store install needs that directory writable and
// persistent, while CLIProxyAPI's working directory often is neither.
func defaultDataDir() string {
	library := ""
	if path := C.model_groups_library_path(); path != nil {
		library = C.GoString(path)
	}
	return dataDirForLibrary(library)
}

func dataDirForLibrary(library string) string {
	library = strings.TrimSpace(library)
	if library == "" {
		abs, errAbs := filepath.Abs(filepath.Join("data", pluginID))
		if errAbs != nil {
			return filepath.Join("data", pluginID)
		}
		return abs
	}
	if !filepath.IsAbs(library) {
		if abs, errAbs := filepath.Abs(library); errAbs == nil {
			library = abs
		}
	}
	dir := filepath.Dir(filepath.Clean(library))
	// The store installs into <plugins>/<goos>/<goarch>/; a manual install may
	// use <plugins>/ or <plugins>/<goos>/.
	candidate := dir
	for range 3 {
		if filepath.Base(candidate) == "plugins" && filepath.Dir(candidate) != candidate {
			return filepath.Join(filepath.Dir(candidate), "data", pluginID)
		}
		candidate = filepath.Dir(candidate)
	}
	return filepath.Join(dir, pluginID+"-data")
}
