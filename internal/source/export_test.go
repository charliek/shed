package source

// WrapWrite replaces how fb saves its file with wrap(the current way), for
// tests in package source_test.
func WrapWrite(fb *FileBindings, wrap func(next func(path string, data []byte) error) func(path string, data []byte) error) {
	fb.write = wrap(fb.write)
}
