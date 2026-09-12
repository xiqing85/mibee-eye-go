package web

import "embed"

// staticFS holds the embedded shared frontend (mibee-webui build:
// index.html, style.css, js/*.js). Source of truth: ../mibee-webui —
// run `make sync-go` there; never hand-edit these files.

//go:embed all:static
var staticFS embed.FS
