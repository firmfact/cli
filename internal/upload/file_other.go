//go:build !unix

package upload

// openFlags are none where there are no FIFOs to wait on.
const openFlags = 0
