package delta

import "io"

// progressTracker tracks total bytes processed across all phases
// and reports percentage based on estimated total work.
type progressTracker struct {
	processed int64
	total     int64
	lastPct   int
	callback  ProgressCallback
}

func newProgressTracker(totalBytes int64, cb ProgressCallback) *progressTracker {
	return &progressTracker{
		total:    totalBytes,
		lastPct:  -1,
		callback: cb,
	}
}

func (pt *progressTracker) add(n int64, label string) {
	pt.processed += n
	if pt.callback == nil || pt.total <= 0 {
		return
	}
	pct := int(pt.processed * 99 / pt.total)
	if pct > 99 {
		pct = 99
	}
	if pct != pt.lastPct {
		pt.lastPct = pct
		pt.callback(pct, label)
	}
}

func (pt *progressTracker) reader(r io.Reader, label string) io.Reader {
	return &trackingReader{r: r, tracker: pt, label: label}
}

type trackingReader struct {
	r       io.Reader
	tracker *progressTracker
	label   string
}

func (tr *trackingReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	if n > 0 {
		tr.tracker.add(int64(n), tr.label)
	}
	return n, err
}
