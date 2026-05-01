// Package imagebuild embeds the recommended sandbox-image
// build context (Dockerfile + matplotlibrc) and the
// version/tag constants the rest of the app uses to
// identify it.
//
// Design: docs/{en,ja}/sandbox-image-build{,.ja}.md.
package imagebuild

import "embed"

// Bundle is the build context shipped with the binary.
// `cliEngine.BuildImage` materialises this into a temp dir
// and runs `podman/docker build` against it.
//
//go:embed all:bundle
var Bundle embed.FS

// BundleVersion MUST be bumped whenever any file under
// bundle/ changes in a way that should invalidate
// previously-built images. The image tag is
// "shell-agent-v2-sandbox:<BundleVersion>" so a new
// version forces a fresh build the next time the user
// clicks Build in Settings.
const BundleVersion = "0.1"

// CanonicalTag is the image tag that Build produces and
// that ImageReady() expects to find on the engine.
const CanonicalTag = "shell-agent-v2-sandbox:" + BundleVersion
