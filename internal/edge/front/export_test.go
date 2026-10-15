package front

import "time"

// SetRefusalSummaryEvery makes f sum up its refused connections every d.
func SetRefusalSummaryEvery(f *Front, d time.Duration) { f.refusalEvery = d }
