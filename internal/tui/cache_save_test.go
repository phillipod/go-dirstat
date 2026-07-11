package tui

import (
	"errors"
	"testing"

	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestCacheSaveCoordinatorNewestSuccessWinsWhenOlderSaveStartsFirst(t *testing.T) {
	var saves cacheSaveCoordinator
	saves.markSuccessful(1)

	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	results := make(chan error, 2)
	var cachedGeneration uint64

	go func() {
		_, err := saves.save(1, func() error {
			cachedGeneration = 1
			close(oldStarted)
			<-releaseOld
			return nil
		})
		results <- err
	}()

	<-oldStarted
	saves.markSuccessful(2)
	go func() {
		_, err := saves.save(2, func() error {
			cachedGeneration = 2
			return nil
		})
		results <- err
	}()

	close(releaseOld)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if cachedGeneration != 2 {
		t.Fatalf("cached generation = %d, want newest successful generation 2", cachedGeneration)
	}
}

func TestCacheSaveCoordinatorSkipsOlderSaveScheduledLast(t *testing.T) {
	var saves cacheSaveCoordinator
	saves.markSuccessful(1)
	saves.markSuccessful(2)

	cachedGeneration := uint64(0)
	if saved, err := saves.save(2, func() error {
		cachedGeneration = 2
		return nil
	}); err != nil || !saved {
		t.Fatalf("new save = (%t, %v), want (true, nil)", saved, err)
	}

	oldCalled := false
	if saved, err := saves.save(1, func() error {
		oldCalled = true
		cachedGeneration = 1
		return nil
	}); err != nil || saved {
		t.Fatalf("old save = (%t, %v), want (false, nil)", saved, err)
	}
	if oldCalled || cachedGeneration != 2 {
		t.Fatalf("stale save ran = %t, cached generation = %d", oldCalled, cachedGeneration)
	}
}

func TestCacheSaveCoordinatorKeepsLastSuccessEligibleAfterNewerScanFailsOrStops(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*testing.T, *model)
	}{
		{
			name: "fails",
			end: func(t *testing.T, m *model) {
				asModel(t, m, scanDoneMsg{generation: 2, err: errors.New("scan failed")})
			},
		},
		{
			name: "stops",
			end: func(_ *testing.T, m *model) {
				m.stopScan()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := New(t.TempDir(), scope.New(), tree.SizeApparent, 0, Options{})
			m := newModel(app)
			m.scanGeneration = 1
			m = asModel(t, m, scanDoneMsg{
				generation: 1,
				node:       &tree.Node{Name: "good", IsDir: true},
			})
			if saved, err := m.cacheSaves.save(0, func() error {
				t.Fatal("save from before the accepted successful scan ran")
				return nil
			}); err != nil || saved {
				t.Fatalf("superseded save = (%t, %v), want (false, nil)", saved, err)
			}

			_ = m.startScan()
			tc.end(t, m)

			called := false
			saved, err := m.cacheSaves.save(1, func() error {
				called = true
				return nil
			})
			if err != nil || !saved || !called {
				t.Fatalf("last good save = (%t, %v), called = %t; want eligible", saved, err, called)
			}
		})
	}
}

func TestCacheSaveCoordinatorReturnsSaveError(t *testing.T) {
	var saves cacheSaveCoordinator
	saves.markSuccessful(1)
	want := errors.New("disk full")

	saved, err := saves.save(1, func() error { return want })
	if !saved || !errors.Is(err, want) {
		t.Fatalf("save = (%t, %v), want (true, disk full)", saved, err)
	}
}
