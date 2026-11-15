// Package closeerr keeps the error from closing a file that was written. A write can
// succeed into the page cache and fail only when the descriptor is closed (a full disk, a
// network filesystem flushing late), so discarding that error installs a truncated file
// with no sign anything went wrong.
package closeerr

import "io"

// Keep closes c and, when *errp is nil, stores the close error there, so the first
// failure wins and a close after a failed write cannot mask it. Defer it in a
// function with a named error result, so it runs on every return path:
//
//	defer closeerr.Keep(&err, f)
func Keep(errp *error, c io.Closer) {
	if cerr := c.Close(); *errp == nil {
		*errp = cerr
	}
}
