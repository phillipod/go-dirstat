//go:build windows

package fsops

// Windows publication uses MOVEFILE_WRITE_THROUGH. Opening directories for
// FlushFileBuffers is not portable through os.File, so no second directory
// handle is required here.
func syncDirectory(string, mutationFilesystem) error { return nil }
