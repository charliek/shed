package vmutil

import "io"

// nopWriteCloser wraps an io.Writer to implement io.WriteCloser.
type nopWriteCloser struct {
	w io.Writer
}

func (n *nopWriteCloser) Write(p []byte) (int, error) {
	return n.w.Write(p)
}

func (n *nopWriteCloser) Close() error {
	return nil
}

// NopWriteCloser wraps an io.Writer to implement io.WriteCloser with a no-op Close.
func NopWriteCloser(w io.Writer) io.WriteCloser {
	return &nopWriteCloser{w: w}
}
