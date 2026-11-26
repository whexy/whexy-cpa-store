package ui

import (
	"os/exec"
	"testing"
)

func TestKitScript(t *testing.T) {
	node, errLook := exec.LookPath("node")
	if errLook != nil {
		t.Skip("node is not installed")
	}
	if out, errRun := exec.Command(node, "--test", "kit_test.mjs").CombinedOutput(); errRun != nil {
		t.Fatalf("kit_test.mjs: %v\n%s", errRun, out)
	}
}
