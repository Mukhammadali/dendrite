//go:build !ios
// +build !ios

package gobind

// logWriter implements io.Writer for logrus output on non-iOS platforms.
type logWriter struct{}

func (l *logWriter) Write(p []byte) (n int, err error) {
	// On non-iOS, just discard logs
	return len(p), nil
}
