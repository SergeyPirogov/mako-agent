//go:build !unix

package fsx

// Lock: without flock there is no lock; the port still keeps a second Mako away.
func Lock(path string) (release func(), err error) {
	return func() {}, nil
}
