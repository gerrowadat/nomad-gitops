// export_test.go exposes unexported functions for testing from the server_test package.
package server

import "time"

// FmtTime wraps fmtTime for external test access.
func FmtTime(t time.Time) string { return fmtTime(t) }
