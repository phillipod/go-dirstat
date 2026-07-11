package fsops

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func FuzzReadPlan(f *testing.F) {
	f.Add([]byte(`{"type":"plan","version":2,"root":"/tmp"}` + "\n"))
	f.Add([]byte(`{"type":"plan","version":2}` + "\n" + `{"type":"operation","id":"x","action":"delete","source":"a"}` + "\n"))
	f.Add([]byte(`{"type":"plan","version":2,"unknown":true}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		_, _ = ReadPlan(bytes.NewReader(data))
	})
}

func FuzzReadOperationRequests(f *testing.F) {
	f.Add([]byte(`{"action":"delete","source":"path"}` + "\n"))
	f.Add([]byte(`{"action":"copy","source":"a","destination":"b"}` + "\n"))
	f.Add([]byte(`{"action":"delete","source":"a","id":"forbidden"}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		_, _ = ReadOperationRequestsLimited(bytes.NewReader(data), 1<<20)
	})
}

func FuzzArchiveValidation(f *testing.F) {
	var validTar bytes.Buffer
	w := tar.NewWriter(&validTar)
	if err := w.WriteHeader(&tar.Header{Name: "safe.txt", Mode: 0o600, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		f.Fatal(err)
	}
	if _, err := w.Write([]byte("safe")); err != nil {
		f.Fatal(err)
	}
	if err := w.Close(); err != nil {
		f.Fatal(err)
	}
	f.Add(uint8(0), validTar.Bytes())
	f.Add(uint8(0), []byte("not a tar archive"))
	f.Add(uint8(1), []byte("not a zip archive"))
	f.Fuzz(func(t *testing.T, formatByte uint8, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		format, extension := archiveFormatTar, ".tar"
		if formatByte%2 == 1 {
			format, extension = "zip", ".zip"
		}
		path := filepath.Join(t.TempDir(), "input"+extension)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = inspectArchive(context.Background(), path, format)
	})
}

func FuzzDirectoryCopyFailureCleanup(f *testing.F) {
	f.Add([]byte("payload"), uint8(0))
	f.Add([]byte("payload"), uint8(1))
	f.Add([]byte("payload"), uint8(2))
	f.Add([]byte("payload"), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, failureMode uint8) {
		if len(data) > 64<<10 {
			return
		}
		root := t.TempDir()
		source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "a"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "b"), append([]byte(nil), data...), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		filesystem := defaultMutationFilesystem()
		copyFile := filesystem.copy
		copies := 0
		switch failureMode % 4 {
		case 1:
			filesystem.copy = func(writer io.Writer, reader io.Reader) (int64, error) {
				copies++
				if copies == 1 {
					return 0, errors.New("injected copy failure")
				}
				return copyFile(writer, reader)
			}
		case 2:
			filesystem.copy = func(writer io.Writer, reader io.Reader) (int64, error) {
				count, err := copyFile(writer, reader)
				cancel()
				return count, err
			}
		case 3:
			publish := filesystem.publish
			filesystem.publish = func(oldPath, newPath string) error {
				if filepath.Base(oldPath) != filepath.Base(source) {
					return errors.New("injected publish failure")
				}
				return publish(oldPath, newPath)
			}
		}
		err := copyPathGuarded(ctx, source, destination, ConflictFail, nil, filesystem)
		if failureMode%4 == 0 {
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		if err == nil {
			t.Fatalf("failure mode %d succeeded", failureMode%4)
		}
		if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
			t.Fatalf("destination visible after %v: %v", err, statErr)
		}
		assertNoStagingArtifacts(t, root)
	})
}

func FuzzRemoveTreeCancellable(f *testing.F) {
	f.Add(uint8(0), uint8(0))
	f.Add(uint8(3), uint8(1))
	f.Add(uint8(8), uint8(4))
	f.Fuzz(func(t *testing.T, entryCount, cancelAt uint8) {
		count := int(entryCount%8) + 1
		root := t.TempDir()
		source := filepath.Join(root, "source")
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < count; index++ {
			path := filepath.Join(source, fmt.Sprintf("entry-%d", index))
			if err := os.WriteFile(path, []byte{byte(index)}, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		threshold := int(cancelAt) % (count + 2)
		if threshold == 0 {
			cancel()
		}
		filesystem := defaultMutationFilesystem()
		remove := filesystem.remove
		removed := 0
		filesystem.remove = func(path string) error {
			if err := remove(path); err != nil {
				return err
			}
			removed++
			if removed == threshold {
				cancel()
			}
			return nil
		}
		err := removeTreeCancellable(ctx, source, filesystem)
		switch {
		case err == nil:
			if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
				t.Fatalf("successful delete left source: %v", statErr)
			}
		case removed > 0 && !isPartialMutation(err):
			t.Fatalf("removed=%d returned non-partial error: %v", removed, err)
		case removed == 0 && isPartialMutation(err):
			t.Fatalf("no removal returned partial error: %v", err)
		}
	})
}
