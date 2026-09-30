package main

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeArch(t *testing.T) {
	cases := map[string]string{
		"aarch64": "aarch64",
		"arm64":   "aarch64",
		"armv7a":  "armv7hl",
		"armv7l":  "armv7hl",
		"armv7hl": "armv7hl",
		"armv7":   "armv7hl",
		"i486":    "i486",
		"i386":    "i486",
		"386":     "i486",
	}
	for in, want := range cases {
		got, err := normalizeArch(in)
		if err != nil || got != want {
			t.Errorf("normalizeArch(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	if _, err := normalizeArch("riscv64"); err == nil {
		t.Fatal("expected error for riscv64")
	}
}

func TestParseArches(t *testing.T) {
	got, err := parseArches("all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "aarch64,armv7hl,i486" {
		t.Fatalf("all: %v", got)
	}
	got, err = parseArches("aarch64, armv7a,i486")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "aarch64,armv7hl,i486" {
		t.Fatalf("list: %v", got)
	}
	got, err = parseArches("i486,i386")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "i486" {
		t.Fatalf("dedup: %v", got)
	}
}

func TestParseArgs(t *testing.T) {
	dir := t.TempDir()
	cfg, err := parseArgs([]string{"5.1.0.11", "armv7a", dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "5.1.0.11" || cfg.Arches[0] != "armv7hl" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.Project != dir {
		t.Fatalf("project %s", cfg.Project)
	}
	if cfg.Output != filepath.Join(dir, "rpms") {
		t.Fatalf("output %s", cfg.Output)
	}

	cfg, err = parseArgs([]string{"-o", "/tmp/rpms", "--rebuild", "5.1.0.11", "aarch64,i486", dir})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Rebuild || cfg.Output != "/tmp/rpms" || len(cfg.Arches) != 2 {
		t.Fatalf("%+v", cfg)
	}

	cfg, err = parseArgs([]string{"--in-place", "--source", "5.1.0.11", "aarch64", dir})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InPlace || !cfg.Source {
		t.Fatal("expected in-place and source")
	}

	cfg, err = parseArgs([]string{"--help"})
	if err != nil || cfg != nil {
		t.Fatalf("help: %v %v", cfg, err)
	}
	if _, err := parseArgs([]string{"5.1.0.11", "aarch64"}); err == nil {
		t.Fatal("expected missing project")
	}
	if _, err := parseArgs([]string{"--nope", "5.1.0.11", "all", dir}); err == nil {
		t.Fatal("expected unknown flag")
	}
}

func TestSpecFieldAndPatch(t *testing.T) {
	yaml := `Name: reversegearhead
Version: 0.0.0.2022.aacs
Sources:
- '%{name}-%{version}.tar.bz2'
`
	s := yaml
	if got := specField(s, "Name"); got != "reversegearhead" {
		t.Fatalf("Name=%q", got)
	}
	if got := specField(s, "Version"); got != "0.0.0.2022.aacs" {
		t.Fatalf("Version=%q", got)
	}
	patched := patchVersion(s, "1.2.3")
	if specField(patched, "Version") != "1.2.3" {
		t.Fatalf("patched version %q", specField(patched, "Version"))
	}
}

func TestFindPkg(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rpm"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rpm", "reversegearhead.yaml"), []byte("Name: reversegearhead\n"), 0644); err != nil {
		t.Fatal(err)
	}
	p, err := findPkg(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "reversegearhead.yaml" {
		t.Fatalf("got %s", p)
	}
	if _, err := findPkg(t.TempDir()); err == nil {
		t.Fatal("expected missing yaml")
	}
}

func TestPrepareStage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rpm"), 0755); err != nil {
		t.Fatal(err)
	}
	yaml := `Name: demo
Version: 1.0
Sources:
- '%{name}-%{version}.tar.bz2'
`
	yamlPath := filepath.Join(dir, "rpm", "demo.yaml")
	if err := os.WriteFile(yamlPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo.so"), []byte("shared"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo.h"), []byte("hdr"), 0644); err != nil {
		t.Fatal(err)
	}

	top, script, cleanup, err := prepareStage(yamlPath, yaml, "demo", "1.0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(top, "rpm", "demo.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(top, "demo.so")); err == nil {
		t.Fatal("should not copy project tree")
	}
	if _, err := os.Stat(filepath.Join(top, "SOURCES", "demo-1.0.tar.bz2")); err == nil {
		t.Fatal("should not pack Source0 tarball")
	}
}

func TestPrepareStageSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.c"), []byte("int main(){}"), 0644); err != nil {
		t.Fatal(err)
	}
	yaml := "Name: srcpkg\nVersion: 2\nSources:\n- '%{name}-%{version}.tar.bz2'\n"
	yamlPath := filepath.Join(dir, "srcpkg.yaml")
	if err := os.WriteFile(yamlPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	top, _, cleanup, err := prepareStage(yamlPath, yaml, "srcpkg", "2", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(top, "rpm", "srcpkg.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(top, "main.c")); err == nil {
		t.Fatal("should not copy project tree")
	}
	if _, err := os.Stat(filepath.Join(top, "SOURCES", "srcpkg-2.tar.bz2")); err == nil {
		t.Fatal("should not pack Source0 tarball")
	}
}

func TestGitArchive(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(dir, "main.c"), []byte("int main(){return 0;}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("Makefile\nharbour-speedtest\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "main.c", ".gitignore")
	run("commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harbour-speedtest"), []byte("bin"), 0644); err != nil {
		t.Fatal(err)
	}

	tarPath, cleanup, err := writeGitArchive(dir, "demo", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(bzip2.NewReader(f))
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names[h.Name] = true
	}
	if !names["demo-1.0/main.c"] {
		t.Fatalf("missing source: %v", names)
	}
	if names["demo-1.0/Makefile"] || names["demo-1.0/harbour-speedtest"] {
		t.Fatalf("archive includes build output: %v", names)
	}
}

func TestGitArchiveSubmodules(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	root := t.TempDir()
	leaf := filepath.Join(root, "leaf")
	mid := filepath.Join(root, "mid")
	parent := filepath.Join(root, "parent")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v: %s", dir, args, out)
		}
	}
	out := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("git -C %s %v: %v", dir, args, err)
		}
		return strings.TrimSpace(string(b))
	}
	initRepo := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		git(dir, "init", "-q")
	}

	initRepo(leaf)
	if err := os.WriteFile(filepath.Join(leaf, "leaf.txt"), []byte("leaf\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(leaf, "add", "leaf.txt")
	git(leaf, "commit", "-qm", "leaf")

	initRepo(mid)
	if err := os.WriteFile(filepath.Join(mid, "mid.txt"), []byte("mid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(mid, "add", "mid.txt")
	git(mid, "commit", "-qm", "mid")
	git(mid, "submodule", "add", "--", leaf, "nested")
	git(mid, "commit", "-qm", "add-nested")

	initRepo(parent)
	if err := os.WriteFile(filepath.Join(parent, "top.txt"), []byte("top\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(parent, "add", "top.txt")
	git(parent, "commit", "-qm", "top")
	git(parent, "submodule", "add", "--", mid, "vendor")
	git(parent, "submodule", "update", "--init", "--recursive")
	git(parent, "commit", "-qm", "add-vendor")

	// URLs in HEAD must not be reachable. Objects stay in .git/modules.
	vendor := filepath.Join(parent, "vendor")
	rewriteGitmodulesURL(t, filepath.Join(vendor, ".gitmodules"), leaf, "https://127.0.0.1:1/leaf.git")
	git(vendor, "add", ".gitmodules")
	git(vendor, "commit", "-qm", "dead-nested-url")
	git(parent, "add", "vendor")
	rewriteGitmodulesURL(t, filepath.Join(parent, ".gitmodules"), mid, "https://127.0.0.1:1/mid.git")
	git(parent, "add", ".gitmodules")
	git(parent, "commit", "-qm", "dead-urls")

	if err := os.WriteFile(filepath.Join(parent, "dirty.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "vendor", "mid.txt"), []byte("dirty-mid\n"), 0644); err != nil {
		t.Fatal(err)
	}

	head := out(parent, "rev-parse", "HEAD")
	status := out(parent, "status", "--porcelain")

	tarPath, cleanup, err := writeGitArchive(parent, "demo", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if got := out(parent, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD changed: %s -> %s", head, got)
	}
	if got := out(parent, "status", "--porcelain"); got != status {
		t.Fatalf("status changed:\n%s\n->\n%s", status, got)
	}

	files := tarFiles(t, tarPath)
	if files["demo-1.0/top.txt"] != "top\n" {
		t.Fatalf("top.txt: %q", files["demo-1.0/top.txt"])
	}
	if files["demo-1.0/vendor/mid.txt"] != "mid\n" {
		t.Fatalf("mid.txt: %q", files["demo-1.0/vendor/mid.txt"])
	}
	if files["demo-1.0/vendor/nested/leaf.txt"] != "leaf\n" {
		t.Fatalf("leaf.txt: %q", files["demo-1.0/vendor/nested/leaf.txt"])
	}
	if _, ok := files["demo-1.0/dirty.txt"]; ok {
		t.Fatal("archive includes untracked dirty.txt")
	}
}

func rewriteGitmodulesURL(t *testing.T, path, from, to string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(from)) {
		t.Fatalf("%s does not contain %s\n%s", path, from, data)
	}
	if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte(from), []byte(to)), 0644); err != nil {
		t.Fatal(err)
	}
}

func tarFiles(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(bzip2.NewReader(f))
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(b)
	}
	return files
}

func TestDockerArgs(t *testing.T) {
	args := dockerBuildArgs("sfosbuild:5.1.0.11-i486", "/ctx", "5.1.0.11", "i486", "linux/386", "abc", "deadbeef")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--platform linux/386",
		"--build-arg SFOS_VERSION=5.1.0.11",
		"--build-arg SFOS_ARCH=i486",
		"--build-arg TARGET_7Z_MD5=abc",
		"--build-arg SFOSBUILD_IMAGE_HASH=deadbeef",
		"-t sfosbuild:5.1.0.11-i486",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}

	run := dockerRunArgs("sfosbuild:5.1.0.11-i486", "linux/386", "i486", "/proj", "/top", "/script.sh", "/out", "/cache/m_root", "/home/user", true, true)
	joined = strings.Join(run, " ")
	for _, want := range []string{
		"run --rm",
		"--network none",
		"--label sfosbuild=1",
		"--platform linux/386",
		"-e SFOS_ARCH=i486",
		"-v /home/user:/home/user:ro",
		"-e SFOS_INPLACE=1",
		"-e SFOS_SOURCE=1",
		"/proj:/build",
		"/top:/rpmbuild",
		"-w /build",
		"/script.sh:/usr/bin/sfos-rpmbuild.sh:ro",
		"/out:/out",
		"/cache/m_root:/root",
		"sfosbuild:5.1.0.11-i486 sh /usr/bin/sfos-rpmbuild.sh",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}

	clean := strings.Join(dockerRunArgs("sfosbuild:5.1.0.11-i486", "linux/386", "i486", "/proj", "/top", "/script.sh", "/out", "/cache/m_root", "/home/user", false, false), " ")
	if strings.Contains(clean, "/proj:/build") || strings.Contains(clean, "SFOS_INPLACE") || strings.Contains(clean, "SFOS_SOURCE") {
		t.Fatalf("clean build should not mount the project: %s", clean)
	}
	if !strings.Contains(clean, "-w /rpmbuild") {
		t.Fatalf("missing rpmbuild workdir in %s", clean)
	}
}

func TestImageTag(t *testing.T) {
	root := "/home/user/work/ReverseGearHead"
	if got := imageRepo("sfosbuild", root); got != "sfosbuild_reversegearhead" {
		t.Fatal(got)
	}
	if got := imageTag("sfosbuild", root, "5.1.0.11", "aarch64"); got != "sfosbuild_reversegearhead:5.1.0.11-aarch64" {
		t.Fatal(got)
	}
	if got := imageBuildTag("sfosbuild", root, "5.1.0.11", "aarch64", "abc123"); got != "sfosbuild_reversegearhead:5.1.0.11-aarch64-abc123" {
		t.Fatal(got)
	}
	if got := sanitizeImageName("My Project!!"); got != "my-project" {
		t.Fatal(got)
	}
	if got := sanitizeImageName("..."); got != "workspace" {
		t.Fatal(got)
	}
}

func TestBuildDepsHash(t *testing.T) {
	meta := "Name: demo\nVersion: 1.0\nBuildRequires: gcc\n"
	h1 := buildDepsHash(meta, "1.0", "sha256:base")
	h2 := buildDepsHash(meta, "2.0", "sha256:base")
	if h1 == h2 || h1 == "" {
		t.Fatalf("hash should differ with version: %q %q", h1, h2)
	}
	h3 := buildDepsHash(meta, "2.0", "sha256:base")
	if h2 != h3 {
		t.Fatalf("hash unstable: %q %q", h2, h3)
	}
	if buildDepsHash(meta, "2.0", "sha256:other") == h2 {
		t.Fatal("hash should differ with base image")
	}
}

func TestFindRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := findRoot(nested); err == nil {
		t.Fatal("expected missing .sfosbuild")
	}
	if err := os.MkdirAll(filepath.Join(root, ".sfosbuild"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := findRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("got %s want %s", got, root)
	}
}

func TestImageHooksDockerfile(t *testing.T) {
	got := imageHooksDockerfile(nil)
	if got != "" {
		t.Fatalf("empty scripts: %q", got)
	}
	got = imageHooksDockerfile([]string{"/proj/.sfosbuild/image/0001-a.sh", "/proj/.sfosbuild/image/0002-b.sh"})
	if !strings.Contains(got, "COPY image-scripts/0001-a.sh /tmp/sfosbuild-image/0001-a.sh") {
		t.Fatalf("missing first copy: %q", got)
	}
	if !strings.Contains(got, "RUN echo \"sfosbuild: image hook 0002-b.sh\" && sh /tmp/sfosbuild-image/0002-b.sh") {
		t.Fatalf("missing second run: %q", got)
	}
	first := strings.Index(got, "0001-a.sh")
	second := strings.Index(got, "0002-b.sh")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("hooks out of order: %q", got)
	}
}

func TestImageScriptsHash(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".sfosbuild", "image")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	empty := imageScriptsHash(root)
	if err := os.WriteFile(filepath.Join(dir, "b.sh"), []byte("echo b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.sh"), []byte("echo a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	h1 := imageScriptsHash(root)
	if h1 == empty || h1 == "" {
		t.Fatal(h1)
	}
	paths := imageScriptPaths(root)
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "a.sh") {
		t.Fatalf("sorted paths %v", paths)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.sh"), []byte("echo a2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	h2 := imageScriptsHash(root)
	if h1 == h2 {
		t.Fatal("hash should change when a script changes")
	}
}

func TestParseShellArgs(t *testing.T) {
	cfg, cmd, err := parseShellArgs([]string{"--rebuild", "5.1.0.11", "armv7a", "sh", "-c", "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Rebuild || cfg.Version != "5.1.0.11" || cfg.Arches[0] != "armv7hl" {
		t.Fatalf("%+v", cfg)
	}
	if strings.Join(cmd, " ") != "sh -c pwd" {
		t.Fatalf("cmd %v", cmd)
	}
	if _, _, err := parseShellArgs([]string{"5.1.0.11", "all"}); err == nil {
		t.Fatal("expected all rejected")
	}
	if _, _, err := parseShellArgs([]string{"5.1.0.11"}); err == nil {
		t.Fatal("expected missing arch")
	}
	cfg, cmd, err = parseShellArgs([]string{"5.1.0.11", "i486"})
	if err != nil || len(cmd) != 0 || cfg.Arches[0] != "i486" {
		t.Fatalf("%v %v %v", cfg, cmd, err)
	}
}

func TestMerRootDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg-cache")
	got := merRootDir("5.1.0.11", "i486")
	want := filepath.Join("/tmp/xdg-cache", "sfosbuild", "5.1.0.11", "i486", "m_root")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got = merRootDir("5.1.0.11", "aarch64")
	want = filepath.Join(home, ".cache", "sfosbuild", "5.1.0.11", "aarch64", "m_root")
	if got != want {
		t.Fatalf("HOME cache: got %s want %s", got, want)
	}
	dir, err := ensureMerRoot("5.1.0.11", "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	if dir != want {
		t.Fatalf("ensure: %s want %s", dir, want)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("mkdir: %v %v", st, err)
	}
}

func TestDockerShellArgs(t *testing.T) {
	wd := "/home/user/work/reversegearhead"
	pwd := wd + "/libreversegearhead"
	args := dockerShellArgs("sfosbuild:5.1.0.11-i486", "linux/386", "i486", wd, pwd, "/cache/m_root", "/home/user", []string{"uname", "-m"}, false)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm",
		"--platform linux/386",
		"-v /home/user:/home/user:ro",
		"-v " + wd + ":" + wd,
		"-v /cache/m_root:/root",
		"-w " + pwd,
		"sfosbuild:5.1.0.11-i486 uname -m",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, " -t ") || strings.Contains(joined, "-it") {
		t.Fatal("no tty")
	}
	if !strings.Contains(joined, " -i ") && !strings.Contains(joined, "run --rm -i") {
		t.Fatalf("stdin should be attached: %s", joined)
	}
	args = dockerShellArgs("sfosbuild:5.1.0.11-i486", "linux/386", "i486", wd, pwd, "/cache/m_root", "/home/user", nil, true)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, " -t") || !strings.HasSuffix(joined, " sh -i") {
		t.Fatalf("interactive sh: %s", joined)
	}
}

func TestParseDeployArgs(t *testing.T) {
	dir := t.TempDir()
	cfg, host, err := parseDeployArgs([]string{"defaultuser@192.168.1.177", dir})
	if err != nil {
		t.Fatal(err)
	}
	if host != "defaultuser@192.168.1.177" || cfg.Project != dir {
		t.Fatalf("%q %+v", host, cfg)
	}
	if cfg.Output != filepath.Join(dir, "rpms") {
		t.Fatalf("output %s", cfg.Output)
	}
	if _, _, err := parseDeployArgs([]string{"nohost", dir}); err == nil {
		t.Fatal("expected user@host error")
	}
	if _, _, err := parseDeployArgs([]string{"user@host"}); err == nil {
		t.Fatal("expected missing project")
	}
}

func TestRemoteVersion(t *testing.T) {
	release := `NAME="Sailfish OS"
VERSION_ID=5.1.0.11
SAILFISH_BUILD=11
`
	if got := osReleaseField(release, "VERSION_ID"); got != "5.1.0.11" {
		t.Fatalf("VERSION_ID=%q", got)
	}
	if got := versionFromProbe("4.5.0.19\n", release); got != "4.5.0.19" {
		t.Fatalf("override=%q", got)
	}
	if got := versionFromProbe("not-a-version\n", release); got != "5.1.0.11" {
		t.Fatalf("invalid override=%q", got)
	}
	if got := versionFromProbe("", release); got != "5.1.0.11" {
		t.Fatalf("no override=%q", got)
	}
}

func TestParseRPMFilename(t *testing.T) {
	name, ver, rel, arch, ok := parseRPMFilename("sailfish-components-gallery-qt5-1.3.0-1.aarch64.rpm")
	if !ok || name != "sailfish-components-gallery-qt5" || ver != "1.3.0" || rel != "1" || arch != "aarch64" {
		t.Fatalf("got %q %q %q %q ok=%v", name, ver, rel, arch, ok)
	}
	name, _, _, _, ok = parseRPMFilename("pkg-debuginfo-1.0-1.aarch64.rpm")
	if !ok || name != "pkg-debuginfo" {
		t.Fatalf("debuginfo name %q ok=%v", name, ok)
	}
	if _, _, _, _, ok = parseRPMFilename("pkg-1.0.rpm"); ok {
		t.Fatal("expected incomplete filename to fail")
	}
}

func TestFindDeployRPMs(t *testing.T) {
	dir := t.TempDir()
	if _, err := findDeployRPMs(dir, "pkg", "1.0", "aarch64"); err == nil {
		t.Fatal("expected empty dir error")
	}
	write := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	main := write("pkg-1.0-1.aarch64.rpm")
	devel := write("pkg-devel-1.0-1.aarch64.rpm")
	doc := write("pkg-doc-1.0-1.noarch.rpm")
	ts := write("pkg-ts-devel-1.0-1.aarch64.rpm")
	debuginfo := write("pkg-debuginfo-1.0-1.aarch64.rpm")
	debugsource := write("pkg-debugsource-1.0-1.aarch64.rpm")
	write("pkg-1.0-1.i486.rpm")
	write("pkg-1-1.aarch64.rpm")
	write("pkg-1.0-1.src.rpm")
	write("other-1.0-1.aarch64.rpm")

	got, err := findDeployRPMs(dir, "pkg", "1.0", "aarch64")
	want := []string{main, devel, doc, ts, debuginfo, debugsource}
	if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q\nwant %q\nerr %v", got, want, err)
	}

	galleryDir := t.TempDir()
	galleryMain := filepath.Join(galleryDir, "sailfish-components-gallery-qt5-1.3.0-1.aarch64.rpm")
	galleryTS := filepath.Join(galleryDir, "sailfish-components-gallery-qt5-ts-devel-1.3.0-1.aarch64.rpm")
	if err := os.WriteFile(galleryMain, []byte("main"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(galleryTS, []byte("ts"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = findDeployRPMs(galleryDir, "sailfish-components-gallery-qt5", "1.3.0", "aarch64")
	want = []string{galleryMain, galleryTS}
	if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gallery: got %q want %q err %v", got, want, err)
	}

	rel2 := write("pkg-1.0-2.aarch64.rpm")
	rel2devel := write("pkg-devel-1.0-2.aarch64.rpm")
	rel2debug := write("pkg-debuginfo-1.0-2.aarch64.rpm")
	past := time.Now().Add(-2 * time.Hour)
	for _, name := range []string{
		"pkg-1.0-1.aarch64.rpm",
		"pkg-devel-1.0-1.aarch64.rpm",
		"pkg-doc-1.0-1.noarch.rpm",
		"pkg-ts-devel-1.0-1.aarch64.rpm",
		"pkg-debuginfo-1.0-1.aarch64.rpm",
		"pkg-debugsource-1.0-1.aarch64.rpm",
	} {
		if err := os.Chtimes(filepath.Join(dir, name), past, past); err != nil {
			t.Fatal(err)
		}
	}
	got, err = findDeployRPMs(dir, "pkg", "1.0", "aarch64")
	want = []string{rel2, rel2devel, rel2debug}
	if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("release: got %q want %q err %v", got, want, err)
	}

	noarchDir := t.TempDir()
	noarch := filepath.Join(noarchDir, "data-2.0-1.noarch.rpm")
	if err := os.WriteFile(noarch, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noarchDir, "data-2.0-1.i486.rpm"), []byte("wrong"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = findDeployRPMs(noarchDir, "data", "2.0", "aarch64")
	want = []string{noarch}
	if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("noarch: got %q want %q err %v", got, want, err)
	}
	archSpecific := filepath.Join(noarchDir, "data-2.0-1.aarch64.rpm")
	if err := os.WriteFile(archSpecific, []byte("bin"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = findDeployRPMs(noarchDir, "data", "2.0", "aarch64")
	want = []string{archSpecific}
	if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("arch over noarch: got %q want %q err %v", got, archSpecific, err)
	}
}

func TestUsage(t *testing.T) {
	var b strings.Builder
	usage(&b)
	s := b.String()
	for _, want := range []string{
		"sfosbuild shell",
		".sfosbuild/",
		".sfosbuild/image/*.sh",
		"-v $wd:$wd",
		"-v m_root:/root",
		"-w $PWD",
		"sfosbuild build",
		"sfosbuild deploy",
		"--in-place",
		"--source",
		"device ~/.sfosbuild-os-version",
		"--network=none",
		"<prefix>_<workspace>",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("help missing %q", want)
		}
	}
}
