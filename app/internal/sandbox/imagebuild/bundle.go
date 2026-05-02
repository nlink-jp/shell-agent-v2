// Package imagebuild owns the recommended Dockerfile body
// the Settings UI shows in its textarea, plus the helpers
// that turn a Dockerfile body into the content-addressed
// image tag used by the rest of the app.
//
// The previous (r1/r2) embed.FS bundle approach was
// removed in r3 — see docs/{en,ja}/sandbox-image-build{,.ja}.md.
package imagebuild

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// TagPrefix is the namespace under which all sandbox
// images live. ListImages filters by `label=<TagPrefix>=1`
// and tag = "<TagPrefix>:<sha[:12]>" so we can enumerate
// our own builds without touching foreign images.
const TagPrefix = "shell-agent-v2-sandbox"

// RecommendedDockerfile is the default Dockerfile body
// shown in the Settings textarea on first open and
// restored by the "Reset to recommended" button. The
// matplotlibrc is created inline (no support files) so
// the build context is a single file.
const RecommendedDockerfile = `FROM python:3.12-slim

# CJK fonts — matplotlib renders Japanese / Chinese /
# Korean labels as tofu without these.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        fonts-noto-cjk \
        fonts-noto-cjk-extra \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Common analysis libs.
RUN pip install --no-cache-dir \
        pandas \
        numpy \
        matplotlib \
        scipy \
        scikit-learn

# matplotlib rcParams: put Noto Sans CJK JP into the font
# fallback chain so charts with Japanese labels render
# correctly even when the script doesn't set rcParams.
RUN mkdir -p /etc/matplotlib && \
    printf 'font.family: sans-serif\nfont.sans-serif: DejaVu Sans, Noto Sans CJK JP, Arial, Liberation Sans\naxes.unicode_minus: False\n' > /etc/matplotlib/matplotlibrc
ENV MATPLOTLIBRC=/etc/matplotlib/matplotlibrc

WORKDIR /work
`

// TagFor returns the content-addressed image tag for a
// given Dockerfile body. Edits to the Dockerfile change
// the tag, so a previous build of a different recipe
// stays on the engine under its own tag (until the user
// removes it from the Settings library).
func TagFor(dockerfile string) string {
	sum := sha256.Sum256([]byte(dockerfile))
	return TagPrefix + ":" + hex.EncodeToString(sum[:6])
}

// digestPinRegex matches an OCI image reference pinned by digest:
// `<registry/path>@sha256:<64 hex>`. Locally-built sandbox images
// (TagPrefix:<sha[:12]>) also count as pinned for this purpose
// because the tag itself is content-addressed and the local engine
// won't pull a different image under that tag.
var digestPinRegex = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)

// IsImageDigestPinned reports whether the given image reference is
// pinned in a way that resists registry / network compromise. Used
// by the Settings UI to surface a "this image is mutable" warning
// (security-hardening-2.md H5). The local content-addressed
// `<TagPrefix>:<sha>` form is treated as pinned because the tag
// matches the build content and the engine never re-fetches it from
// a registry.
func IsImageDigestPinned(image string) bool {
	if image == "" {
		return false
	}
	if digestPinRegex.MatchString(image) {
		return true
	}
	// Locally-built sandbox image: tag is sha-prefixed and the
	// engine never re-pulls.
	if len(image) > len(TagPrefix)+1 && image[:len(TagPrefix)+1] == TagPrefix+":" {
		return true
	}
	return false
}
