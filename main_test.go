package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	top, script, cleanup, err := prepareStage(yamlPath, yaml, "demo", "1.0")
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
	top, _, cleanup, err := prepareStage(yamlPath, yaml, "srcpkg", "2")
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

	run := dockerRunArgs("sfosbuild:5.1.0.11-i486", "linux/386", "i486", "/proj", "/top", "/script.sh", "/out", "/cache/m_root")
	joined = strings.Join(run, " ")
	for _, want := range []string{
		"run --rm",
		"--label sfosbuild=1",
		"--platform linux/386",
		"-e SFOS_ARCH=i486",
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
	h1 := buildDepsHash(meta, "1.0")
	h2 := buildDepsHash(meta, "2.0")
	if h1 == h2 || h1 == "" {
		t.Fatalf("hash should differ with version: %q %q", h1, h2)
	}
	h3 := buildDepsHash(meta, "2.0")
	if h2 != h3 {
		t.Fatalf("hash unstable: %q %q", h2, h3)
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
	args := dockerShellArgs("sfosbuild:5.1.0.11-i486", "linux/386", "i486", wd, pwd, "/cache/m_root", []string{"uname", "-m"}, false)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm",
		"--platform linux/386",
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
	args = dockerShellArgs("sfosbuild:5.1.0.11-i486", "linux/386", "i486", wd, pwd, "/cache/m_root", nil, true)
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

func TestFindMainRPM(t *testing.T) {
	dir := t.TempDir()
	if _, err := findMainRPM(dir, "pkg"); err == nil {
		t.Fatal("expected empty dir error")
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg-1.0-debuginfo.rpm"), []byte("debug"), 0644); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "pkg-1.0.rpm")
	if err := os.WriteFile(main, []byte("pkg"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := findMainRPM(dir, "pkg")
	if err != nil || got != main {
		t.Fatalf("got %q err %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg-ts-devel-1.0.rpm"), []byte("ts"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = findMainRPM(dir, "pkg")
	if err != nil || got != main {
		t.Fatalf("subpkg skip: got %q err %v", got, err)
	}
	galleryMain := filepath.Join(dir, "sailfish-components-gallery-qt5-1.3.0-1.aarch64.rpm")
	if err := os.WriteFile(galleryMain, []byte("main"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sailfish-components-gallery-qt5-ts-devel-1.3.0-1.aarch64.rpm"), []byte("ts"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = findMainRPM(dir, "sailfish-components-gallery-qt5")
	if err != nil || got != galleryMain {
		t.Fatalf("got %q want %q err %v", got, galleryMain, err)
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
		"device ~/.sfosbuild-os-version",
		"<prefix>_<workspace>",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("help missing %q", want)
		}
	}
}
