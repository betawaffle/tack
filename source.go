package main

import (
	"context"
	"embed"
	"io/fs"
	"time"

	"github.com/betawaffle/tack/internal/prompt"
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
// with these settings.
func variables(settings store.Settings) map[string]string {
	out := tools.Variables(context.Background(), settings)
	out["date"] = prompt.Date(time.Now())
	out["model"] = settings.Model
	out["host"] = settings.Host
	out["tack_revision"] = version()
	return out
}
