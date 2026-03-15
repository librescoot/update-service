package delta

import (
	"io"
	"time"
)

// progressReader wraps an io.Reader and reports progress based on bytes read.
type progressReader struct {
	r          io.Reader
	total      int64
	read       int64
	pctStart   int
	pctEnd     int
	lastReport time.Time
	interval   time.Duration
	callback   ProgressCallback
	label      string
}

func newProgressReader(r io.Reader, total int64, pctStart, pctEnd int, label string, cb ProgressCallback) *progressReader {
	return &progressReader{
		r:        r,
		total:    total,
		pctStart: pctStart,
		pctEnd:   pctEnd,
		interval: 5 * time.Second,
		callback: cb,
		label:    label,
	}
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.read += int64(n)

	if pr.callback != nil && pr.total > 0 && time.Since(pr.lastReport) >= pr.interval {
		frac := float64(pr.read) / float64(pr.total)
		if frac > 1 {
			frac = 1
		}
		pct := pr.pctStart + int(frac*float64(pr.pctEnd-pr.pctStart))
		pr.callback(pct, pr.label)
		pr.lastReport = time.Now()
	}

	return n, err
}
