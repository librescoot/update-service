package delta

import (
	"io"
)

// progressReader wraps an io.Reader and reports progress based on bytes read.
// Only fires the callback when the integer percentage actually changes.
type progressReader struct {
	r        io.Reader
	total    int64
	read     int64
	pctStart int
	pctEnd   int
	lastPct  int
	callback ProgressCallback
	label    string
}

func newProgressReader(r io.Reader, total int64, pctStart, pctEnd int, label string, cb ProgressCallback) *progressReader {
	return &progressReader{
		r:        r,
		total:    total,
		pctStart: pctStart,
		pctEnd:   pctEnd,
		lastPct:  -1,
		callback: cb,
		label:    label,
	}
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.read += int64(n)

	if pr.callback != nil && pr.total > 0 {
		frac := float64(pr.read) / float64(pr.total)
		if frac > 1 {
			frac = 1
		}
		pct := pr.pctStart + int(frac*float64(pr.pctEnd-pr.pctStart))
		if pct != pr.lastPct {
			pr.callback(pct, pr.label)
			pr.lastPct = pct
		}
	}

	return n, err
}
