package diagnose

import (
	"context"
	"runtime"
	"testing"
)

func TestGatherReportsVolumeAndCapability(t *testing.T) {
	result := Gather(context.Background(), []string{t.TempDir()})
	if len(result.Volumes) != 1 {
		t.Fatalf("volumes = %d, want 1 (warnings: %v)", len(result.Volumes), result.Warnings)
	}
	if len(result.Capabilities) != 1 || result.Capabilities[0].Name != "open-deleted-files" {
		t.Fatalf("capabilities = %#v", result.Capabilities)
	}
	if runtime.GOOS != "linux" && result.Capabilities[0].Available {
		t.Fatal("open-deleted capability unexpectedly available")
	}
}

func TestGatherHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := Gather(ctx, []string{t.TempDir()})
	if len(result.Warnings) == 0 {
		t.Fatal("cancelled gather did not report cancellation")
	}
}
