package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGoBuilderProducesBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module hello\n\ngo 1.24\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\nimport \"fmt\"\nfunc main() { fmt.Print(\"hello from oozie\") }\n")

	bin, err := (GoBuilder{}).Build(dir, "Hello Oozie")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if filepath.Base(bin) != "hello-oozie" {
		t.Errorf("binary name = %s, want hello-oozie", filepath.Base(bin))
	}
	info, err := os.Stat(bin)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("binary missing or not executable: %v", err)
	}
	out, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatalf("run binary: %v", err)
	}
	if string(out) != "hello from oozie" {
		t.Errorf("output = %q", out)
	}
}

func TestBuildFailsWithoutModule(t *testing.T) {
	_, err := (GoBuilder{}).Build(t.TempDir(), "Nope")
	if err == nil {
		t.Fatal("expected error for empty project")
	}
}

func TestBuildable(t *testing.T) {
	dir := t.TempDir()
	if Buildable(dir) {
		t.Fatal("empty dir should not be buildable")
	}
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.24\n")
	if Buildable(dir) {
		t.Fatal("go.mod alone should not be buildable")
	}
	writeFile(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
	if !Buildable(dir) {
		t.Fatal("go.mod + main.go should be buildable")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
