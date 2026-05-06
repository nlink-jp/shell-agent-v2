// objects.go: bundle integration with the global object store.
//
// The bundle includes any objstore objects owned by the exported
// session so the imported session can render attached images and
// open generated reports. ID strategy is "always regenerate on
// import" — see design §5.3 for why.

package sessionio

import (
	"io"

	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

// ObjectExport is one object queued for inclusion in a bundle.
// Open is lazy so a session with hundreds of objects does not
// pin all blobs in RAM at once during export.
type ObjectExport struct {
	Meta *objstore.ObjectMeta
	Open func() (io.ReadCloser, error)
}

// ObjstoreWriter is the subset of *objstore.Store that ImportSession
// uses to register bundled objects under fresh IDs. Defining it as
// an interface lets unit tests swap in a fake without touching the
// real on-disk store, and keeps this package free of any objstore
// state-management concerns.
type ObjstoreWriter interface {
	Store(reader io.Reader, objType objstore.ObjectType, mimeType, origName, sessionID string) (*objstore.ObjectMeta, error)
	Delete(id string) error
}

// objectIndexEntry is the per-object record carried in the bundle's
// `objects/index.json`. It is a lossy projection of objstore.ObjectMeta:
// SessionID is dropped (the bundle is itself session-scoped) and ID
// is preserved only so the importer can build the old→new ID map.
type objectIndexEntry struct {
	ID        string              `json:"id"`
	Type      objstore.ObjectType `json:"type"`
	MimeType  string              `json:"mime_type"`
	OrigName  string              `json:"orig_name"`
	CreatedAt string              `json:"created_at"`
	Size      int64               `json:"size"`
}
