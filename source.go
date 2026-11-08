package main

import (
	"context"
	"embed"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/betawaffle/tack/internal/prompt"
	"github.com/betawaffle/tack/internal/pyenv"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tools"
)

// source is every file in the repository, as this binary was built from it, for the
// codebase and documentation pages. tack-deploy only builds what's committed, so it's
// exactly the deployed revision. TestTheSourceIsEveryTrackedFile keeps the list whole.
//
//go:embed *.go go.mod README.md LICENSE tack.service .gitignore
//go:embed cmd internal docs
//go:embed test/browser/*.js test/browser/*.sh test/browser/*.json test/browser/.gitignore test/browser/toolserver
var source embed.FS

// pythonSite is the packages code can import, if they've been built: tack-deploy builds them
// from the lock before tack starts.
func pythonSite(dataDir string) (site, pythonFor string) {
	lock := pyenv.Default()
	root := filepath.Join(dataDir, "pyenv")
	if !lock.Built(root) {
		return "", lock.Python
	}
	return pyenv.Site(lock.Dir(root)), lock.Python
}

// modelDocsDir is where tack's documentation for models is, in the repository.
const modelDocsDir = "docs/model"

func modelDocs() fs.FS {
	sub, err := fs.Sub(source, modelDocsDir)
	if err != nil {
		panic(err)
	}
	return sub
}

// variables are what {{name}} fills with in prompts and documentation, for a conversation
// with these settings, when code can import the packages in site.
func variables(site string) func(store.Settings) map[string]string {
	return func(settings store.Settings) map[string]string {
		return variablesFor(settings, site)
	}
}

func variablesFor(settings store.Settings, site string) map[string]string {
	out := tools.Variables(context.Background(), settings, site)
	out["date"] = prompt.Date(time.Now())
	out["model"] = settings.Model
	out["host"] = settings.Host
	out["tack_revision"] = version()
	return out
}
