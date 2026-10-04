package github

import "time"

// SetClock replaces the App's clock (token expiry tests).
func SetClock(a *App, now func() time.Time) { a.now = now }
