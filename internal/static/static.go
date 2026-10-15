// Package static holds the app's own web files: its icons and its settings sections (account, import, export).
package static

import "embed"

//go:embed favicon.svg apple-touch-icon.png account.js import.js export.js upload.js
var FS embed.FS
