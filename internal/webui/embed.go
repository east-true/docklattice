package webui

import "embed"

// Embed the directory as one tree rather than matching each immediate child as
// its own pattern. Empty local subdirectories are then ignored, while future
// nested assets remain supported.
//
//go:embed assets
var embeddedAssets embed.FS
