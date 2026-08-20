package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestFsReadTool_AwkCommandHasNoDoubleDash is a regression test for a
// bug found running a real job against a real LLM: the workspace
// image's awk (mawk) does not treat "--" as an end-of-options marker
// and instead tries to open a file literally named "--", failing
// every fs_read call. See fs.go's awk command comment.
func TestFsReadTool_AwkCommandHasNoDoubleDash(t *testing.T) {
	sb := newFakeSandbox()
	tool := fsReadTool(sb)

	args, _ := json.Marshal(map[string]string{"path": "app/main.go"})
	if _, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}

	var awkCmd string
	for _, cmd := range sb.commands {
		if strings.HasPrefix(cmd, "awk ") {
			awkCmd = cmd
		}
	}
	if awkCmd == "" {
		t.Fatal("no awk command was executed")
	}
	if strings.Contains(awkCmd, " -- ") {
		t.Errorf("awk command = %q, must not contain a \"--\" end-of-options marker (mawk does not support it)", awkCmd)
	}
}

func TestFsListTool_ResolvesRelativePathUnderWorkspaceRoot(t *testing.T) {
	sb := newFakeSandbox()
	tool := fsListTool(sb)

	args, _ := json.Marshal(map[string]string{"path": "app"})
	if _, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}

	var findCmd string
	for _, cmd := range sb.commands {
		if strings.HasPrefix(cmd, "find ") {
			findCmd = cmd
		}
	}
	if findCmd == "" {
		t.Fatal("no find command was executed")
	}
	if !strings.Contains(findCmd, "/workspace/app") {
		t.Errorf("find command = %q, want it to reference /workspace/app", findCmd)
	}
	if strings.Contains(findCmd, "/workspace/workspace") {
		t.Errorf("find command = %q, workspace root is duplicated", findCmd)
	}
}

func TestFsWriteTool_WritesViaBase64(t *testing.T) {
	sb := newFakeSandbox()
	tool := fsWriteTool(sb)

	args, _ := json.Marshal(map[string]string{"path": "app/main.go", "content": "package main"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if !strings.Contains(obs, "wrote") {
		t.Errorf("observation = %q, want a byte-count confirmation", obs)
	}

	var writeCmd string
	for _, cmd := range sb.commands {
		if strings.Contains(cmd, "base64 -d") {
			writeCmd = cmd
		}
	}
	if writeCmd == "" {
		t.Fatal("no base64-decoding write command was executed")
	}
}
