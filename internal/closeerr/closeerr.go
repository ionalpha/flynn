// Package closeerr keeps the error from closing a file that was written. A write can
// succeed into the page cache and fail only when the descriptor is closed (a full disk, a
// network filesystem flushing late), so discarding that error installs a truncated file
// with no sign anything went wrong.
package closeerr

import "io"

// Keep closes c and, when *errp is still nil, stores the close error there, so the first
// failure wins and a close that follows a failed write cannot mask it. Call it deferred on
// a function with a named error result:
//
//	defer closeerr.Keep(&err, f)
//
// The deferred form runs on every return path, which is why a write path uses it instead
// of closing by hand at the end of the happy path.
func Keep(errp *error, c io.Closer) {
	if cerr := c.Close(); *errp == nil {
		*errp = cerr
	}
}
