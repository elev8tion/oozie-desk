// Package oozie embeds the UI assets and migrations so the compiled
// binary is fully self-contained and runs from anywhere.
package oozie

import "embed"

//go:embed templates static migrations
var Assets embed.FS
