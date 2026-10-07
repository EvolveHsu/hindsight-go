package server

import "time"

// timeTime is a local alias so engine.go does not import time directly.
type timeTime = time.Time

// timeRFC3339 is the primary ISO-8601 layout the upstream API documents.
const timeRFC3339 = time.RFC3339

// timeParse delegates to time.Parse.
func timeParse(layout, value string) (time.Time, error) { return time.Parse(layout, value) }
