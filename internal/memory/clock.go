package memory

import "time"

// timeNow is a seam so tests can freeze the clock.
var timeNow = time.Now
