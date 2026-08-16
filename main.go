package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

//go:embed Dockerfile unpack.sh prepare.sh rpmbuild.sh install-builddeps.sh
var embedded embed.FS

const dockerRunLabel = "sfosbuild=1"

var knownMD5 = map[string]string{
	"5.1.0.11/aarch64": "24b40e5e6c1366996dc5a4a69a46e3b3",
	"5.1.0.11/armv7hl": "a2ad4336d5466cced44d57c21c32e8ce",
	"5.1.0.11/i486":    "753619f8d84c6c6d7e83f37ee735c298",
}

var archPlatform = map[string]string{
	"aarch64": "linux/arm64",
	"armv7hl": "linux/arm/v7",
	"i486":    "linux/386",
}

type config struct {
	Version     string
	Arches      []string
	Project     string
	Output      string
	Rebuild     bool
	ImagePrefix string
	Root        string // $wd: directory that contains .sfosbuild/
	WorkDir     string // host cwd, used as docker -w
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("sfosbuild: ")
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "shell" {
		return runShell(args[1:])
	}
	if len(args) > 0 && args[0] == "deploy" {
		return runDeploy(args[1:])
	}
	if len(args) > 0 && args[0] == "build" {
		args = args[1:]
	}
	cfg, err := parseArgs(args)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	return runBuild(cfg)
}

func runBuild(cfg *config) error {
	if err := withWorkspace(cfg); err != nil {
		return err
	}
	pkgPath, err := findPkg(cfg.Project)
	if err != nil {
		return err
	}
	meta, err := os.ReadFile(pkgPath)
	if err != nil {
		return err
	}
	name := specField(string(meta), "Name")
	if name == "" {
		return fmt.Errorf("no Name: in %s", pkgPath)
	}
	version := os.Getenv("RPM_VERSION")
	if version == "" {
		version = specField(string(meta), "Version")
	}
	if version == "" {
		return fmt.Errorf("no Version: in %s (or RPM_VERSION)", pkgPath)
	}

	if err := os.MkdirAll(cfg.Output, 0755); err != nil {
		return err
	}
	outAbs, err := filepath.Abs(cfg.Output)
	if err != nil {
		return err
	}

	for _, arch := range cfg.Arches {
		log.Printf("%s %s %s", cfg.Version, arch, name)
		if err := buildArch(cfg, arch, pkgPath, string(meta), name, version, outAbs); err != nil {
			return fmt.Errorf("%s: %w", arch, err)
		}
	}
	log.Printf("rpms in %s", outAbs)
	return nil
}

func runDeploy(args []string) error {
	cfg, userHost, err := parseDeployArgs(args)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	arch, err := remoteArch(userHost)
	if err != nil {
		return fmt.Errorf("%s arch: %w", userHost, err)
	}
	version, err := remoteVersion(userHost)
	if err != nil {
		return fmt.Errorf("%s version: %w", userHost, err)
	}
	log.Printf("%s is %s %s", userHost, arch, version)
	cfg.Version = version
	cfg.Arches = []string{arch}
	if err := runBuild(cfg); err != nil {
		return err
	}
	outAbs, err := filepath.Abs(cfg.Output)
	if err != nil {
		return err
	}
	pkgPath, err := findPkg(cfg.Project)
	if err != nil {
		return err
	}
	meta, err := os.ReadFile(pkgPath)
	if err != nil {
		return err
	}
	pkgName := specField(string(meta), "Name")
	if pkgName == "" {
		return fmt.Errorf("no Name: in %s", pkgPath)
	}
	rpm, err := findMainRPM(outAbs, pkgName)
	if err != nil {
		return err
	}
	return installRPM(userHost, rpm)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  sfosbuild [options] <sfos-version> <arch[,arch...]|all> <project>
  sfosbuild build [options] <sfos-version> <arch[,arch...]|all> <project>
  sfosbuild shell [options] <sfos-version> <arch> [command...]
  sfosbuild deploy [options] <user@host> <project>

Build Sailfish OS RPMs, deploy to a device, or open a shell in SDK target containers.

Workspace:
  Walks up from the current directory looking for .sfosbuild/. That directory
  is $wd (the workspace root). If it is not found, sfosbuild exits. Every
  docker build and run is done in that workspace: image hooks come from
  $wd/.sfosbuild/image/, and shell mounts $wd at the same path.

  .sfosbuild/image/*.sh
      Run during SDK image build, in sorted order, after the target is
      unpacked. The image is rebuilt when these scripts change (or with
      --rebuild).

Commands:
  build (default)  Package a project with specify + rpmbuild
  deploy           ssh uname -m and /etc/os-release, build, copy RPM, install on device
  shell            docker run -i -t -v $wd:$wd -w $PWD in the SDK image
                   No command -> interactive sh -i. Rebuilds the image if hooks changed.

Examples:
  sfosbuild 5.1.0.11 aarch64 ./my_sfos_source
  sfosbuild 5.1.0.11 all ./my_sfos_source
  sfosbuild shell 5.1.0.11 aarch64
  sfosbuild shell 5.1.0.11 i486 uname -m
  sfosbuild shell 5.1.0.11 i486 sh -c 'pwd; ls'
  sfosbuild deploy defaultuser@192.168.1.177 ./my_sfos_source

Options:
  -o, --output DIR     RPM output directory (default: <project>/rpms)
  --rebuild            rebuild SDK images even if they exist
  --image-prefix NAME  docker image prefix (default: sfosbuild)
  -h, --help           show this help

Architectures: aarch64, armv7hl (armv7a), i486, all
  shell takes a single arch (not all).

The project needs rpm/*.yaml (spectacle). specify runs in the SDK image
to generate the spec. The project directory is packed as Source0.
Images are tagged <prefix>:<version>-<arch>.
`)
}

func parseFlags(args []string) (*config, []string, error) {
	cfg := &config{ImagePrefix: getenv("SFOSBUILD_IMAGE_PREFIX", "sfosbuild")}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			break
		}
		switch {
		case a == "-h" || a == "--help":
			usage(os.Stdout)
			return nil, nil, nil
		case a == "--rebuild":
			cfg.Rebuild = true
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs an argument", a)
			}
			i++
			cfg.Output = args[i]
		case a == "--image-prefix":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs an argument", a)
			}
			i++
			cfg.ImagePrefix = args[i]
		case strings.HasPrefix(a, "-o"):
			cfg.Output = strings.TrimPrefix(a, "-o")
		default:
			return nil, nil, fmt.Errorf("unknown flag %s", a)
		}
	}
	return cfg, args[i:], nil
}

func parseArgs(args []string) (*config, error) {
	cfg, pos, err := parseFlags(args)
	if err != nil || cfg == nil {
		return cfg, err
	}
	if len(pos) != 3 {
		usage(os.Stderr)
		return nil, fmt.Errorf("want: <sfos-version> <arch|all> <project>")
	}
	cfg.Version = pos[0]
	arches, err := parseArches(pos[1])
	if err != nil {
		return nil, err
	}
	cfg.Arches = arches
	abs, err := filepath.Abs(pos[2])
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", abs)
	}
	cfg.Project = abs
	if cfg.Output == "" {
		cfg.Output = filepath.Join(abs, "rpms")
	}
	return cfg, nil
}

func parseDeployArgs(args []string) (*config, string, error) {
	cfg, pos, err := parseFlags(args)
	if err != nil || cfg == nil {
		return cfg, "", err
	}
	if len(pos) != 2 {
		usage(os.Stderr)
		return nil, "", fmt.Errorf("want: sfosbuild deploy <user@host> <project>")
	}
	userHost := pos[0]
	if !strings.Contains(userHost, "@") {
		return nil, "", fmt.Errorf("want user@host, got %q", userHost)
	}
	abs, err := filepath.Abs(pos[1])
	if err != nil {
		return nil, "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, "", err
	}
	if !st.IsDir() {
		return nil, "", fmt.Errorf("not a directory: %s", abs)
	}
	cfg.Project = abs
	if cfg.Output == "" {
		cfg.Output = filepath.Join(abs, "rpms")
	}
	return cfg, userHost, nil
}

func parseShellArgs(args []string) (*config, []string, error) {
	cfg, pos, err := parseFlags(args)
	if err != nil || cfg == nil {
		return cfg, nil, err
	}
	if len(pos) < 2 {
		usage(os.Stderr)
		return nil, nil, fmt.Errorf("want: sfosbuild shell <sfos-version> <arch> [command...]")
	}
	if pos[1] == "all" {
		return nil, nil, fmt.Errorf("shell needs a single arch, not all")
	}
	cfg.Version = pos[0]
	arch, err := normalizeArch(pos[1])
	if err != nil {
		return nil, nil, err
	}
	cfg.Arches = []string{arch}
	return cfg, pos[2:], nil
}

func withWorkspace(cfg *config) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg.WorkDir = cwd
	root, err := findRoot(cwd)
	if err != nil {
		return err
	}
	cfg.Root = root
	rel, err := filepath.Rel(root, cwd)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("cwd %s is not inside workspace %s", cwd, root)
	}
	log.Printf("workspace %s", root)
	return nil
}

func findRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		st, err := os.Stat(filepath.Join(dir, ".sfosbuild"))
		if err == nil && st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .sfosbuild/ (walked up from %s)", start)
		}
		dir = parent
	}
}

func imageScriptPaths(root string) []string {
	matches, err := filepath.Glob(filepath.Join(root, ".sfosbuild", "image", "*.sh"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

const imageHooksMarker = "# @@sfosbuild-image-hooks@@\n"

func imageHooksDockerfile(scripts []string) string {
	if len(scripts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range scripts {
		base := filepath.Base(s)
		fmt.Fprintf(&b, "COPY image-scripts/%s /tmp/sfosbuild-image/%s\n", base, base)
		fmt.Fprintf(&b, "RUN echo \"sfosbuild: image hook %s\" && sh /tmp/sfosbuild-image/%s\n\n", base, base)
	}
	return b.String()
}

func imageScriptsHash(root string) string {
	h := sha256.New()
	for _, f := range imageScriptPaths(root) {
		io.WriteString(h, filepath.Base(f))
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func runShell(args []string) error {
	cfg, cmd, err := parseShellArgs(args)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	if err := withWorkspace(cfg); err != nil {
		return err
	}
	arch := cfg.Arches[0]
	if err := ensureImage(cfg, arch); err != nil {
		return err
	}
	tag := imageTag(cfg.ImagePrefix, cfg.Version, arch)
	dargs := dockerShellArgs(tag, archPlatform[arch], arch, cfg.Root, cfg.WorkDir, cmd, len(cmd) == 0)
	c := dockerCmd(dargs...)
	c.Stdin = os.Stdin
	return runCmd(c)
}

func dockerShellArgs(tag, platform, arch, wd, workdir string, cmd []string, tty bool) []string {
	args := []string{"run", "--rm", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args,
		"--label", dockerRunLabel,
		"--platform", platform,
		"-e", "SFOS_ARCH="+arch,
		"-v", wd+":"+wd,
		"-w", workdir,
		tag,
	)
	if len(cmd) == 0 {
		args = append(args, "sh", "-i")
	} else {
		args = append(args, cmd...)
	}
	return args
}

func parseArches(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "all" {
		return []string{"aarch64", "armv7hl", "i486"}, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		a, err := normalizeArch(p)
		if err != nil {
			return nil, err
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no architectures")
	}
	return out, nil
}

func normalizeArch(s string) (string, error) {
	switch strings.ToLower(s) {
	case "aarch64", "arm64":
		return "aarch64", nil
	case "armv7hl", "armv7a", "armv7l", "armv7", "arm":
		return "armv7hl", nil
	case "i486", "i386", "386", "x86":
		return "i486", nil
	default:
		return "", fmt.Errorf("unknown arch %q (aarch64, armv7hl, i486, all)", s)
	}
}

func imageTag(prefix, version, arch string) string {
	return prefix + ":" + version + "-" + arch
}

func imageBuildTag(prefix, version, arch, depsHash string) string {
	return fmt.Sprintf("%s:%s-%s-%s", prefix, version, arch, depsHash)
}

func buildDepsHash(meta, version string) string {
	h := sha256.New()
	h.Write([]byte(patchVersion(meta, version)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func findPkg(project string) (string, error) {
	for _, g := range []string{
		filepath.Join(project, "rpm", "*.yaml"),
		filepath.Join(project, "*.yaml"),
		filepath.Join(project, "rpm", "*.spec"),
		filepath.Join(project, "*.spec"),
	} {
		matches, err := filepath.Glob(g)
		if err != nil {
			return "", err
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return "", fmt.Errorf("multiple package files in %s", project)
		}
	}
	return "", fmt.Errorf("no rpm/*.yaml in %s", project)
}

func specField(spec, field string) string {
	prefix := strings.ToLower(field) + ":"
	for _, line := range strings.Split(spec, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(trim), prefix) {
			return strings.TrimSpace(trim[len(field)+1:])
		}
	}
	return ""
}

func osReleaseField(content, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, prefix) {
			v := strings.TrimSpace(trim[len(prefix):])
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func patchVersion(spec, version string) string {
	lines := strings.Split(spec, "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trim), "version:") {
			lines[i] = "Version:    " + version
			break
		}
	}
	return strings.Join(lines, "\n")
}

func sourceArchive(meta, name, version string) string {
	s := specField(meta, "Source0")
	if s == "" {
		s = specField(meta, "Source")
	}
	if s == "" {
		for _, line := range strings.Split(meta, "\n") {
			t := strings.TrimSpace(line)
			t = strings.TrimPrefix(t, "- ")
			t = strings.Trim(t, `"'`)
			if strings.Contains(t, ".tar.") {
				s = t
				break
			}
		}
	}
	s = strings.ReplaceAll(s, "%{name}", name)
	s = strings.ReplaceAll(s, "%{version}", version)
	if s == "" {
		s = name + "-" + version + ".tar.bz2"
	}
	return s
}

func tarFlag(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".tar.bz2"), strings.HasSuffix(filename, ".tbz2"):
		return "-cjf"
	case strings.HasSuffix(filename, ".tar.gz"), strings.HasSuffix(filename, ".tgz"):
		return "-czf"
	case strings.HasSuffix(filename, ".tar.xz"):
		return "-cJf"
	default:
		return "-cf"
	}
}

func cp(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

func copyTree(src, dst string) error {
	skip := map[string]bool{".git": true, "dist": true, "rpms": true, "out": true, "vendor": true}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0755)
		}
		top := strings.Split(rel, string(filepath.Separator))[0]
		if skip[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return cp(path, target)
	})
}

func prepareStage(project, pkgPath, meta, name, version string) (topdir, script string, cleanup func(), err error) {
	work, err := os.MkdirTemp("", "sfosbuild-")
	if err != nil {
		return "", "", nil, err
	}
	cleanup = func() {
		if os.Getenv("SFOSBUILD_KEEP") != "" {
			log.Printf("keeping %s", work)
			return
		}
		os.RemoveAll(work)
	}
	topdir = filepath.Join(work, "rpmbuild")
	for _, d := range []string{"SPECS", "SOURCES", "BUILD", "RPMS", "SRPMS"} {
		if err := os.MkdirAll(filepath.Join(topdir, d), 0755); err != nil {
			cleanup()
			return "", "", nil, err
		}
	}
	folder := name + "-" + version
	payload := filepath.Join(work, folder)
	if err := copyTree(project, payload); err != nil {
		cleanup()
		return "", "", nil, err
	}

	src0 := sourceArchive(meta, name, version)
	tarPath := filepath.Join(topdir, "SOURCES", src0)
	if err := os.MkdirAll(filepath.Dir(tarPath), 0755); err != nil {
		cleanup()
		return "", "", nil, err
	}
	cmd := exec.Command("tar", "-C", work, tarFlag(src0), tarPath, folder)
	if err := runCmd(cmd); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("tar: %w", err)
	}

	patched := patchVersion(meta, version)
	if strings.HasSuffix(pkgPath, ".yaml") {
		if err := os.MkdirAll(filepath.Join(topdir, "rpm"), 0755); err != nil {
			cleanup()
			return "", "", nil, err
		}
		yamlName := name + ".yaml"
		if err := os.WriteFile(filepath.Join(topdir, "rpm", yamlName), []byte(patched), 0644); err != nil {
			cleanup()
			return "", "", nil, err
		}
		if err := os.WriteFile(filepath.Join(topdir, "SOURCES", yamlName), []byte(patched), 0644); err != nil {
			cleanup()
			return "", "", nil, err
		}
	} else {
		if err := os.WriteFile(filepath.Join(topdir, "SPECS", name+".spec"), []byte(patched), 0644); err != nil {
			cleanup()
			return "", "", nil, err
		}
	}

	scriptBytes, err := embedded.ReadFile("rpmbuild.sh")
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	script = filepath.Join(work, "rpmbuild.sh")
	if err := os.WriteFile(script, scriptBytes, 0755); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return topdir, script, cleanup, nil
}

func ensureImage(cfg *config, arch string) error {
	tag := imageTag(cfg.ImagePrefix, cfg.Version, arch)
	hash := imageScriptsHash(cfg.Root)
	if !cfg.Rebuild && imageExists(tag) && imageHash(tag) == hash {
		log.Printf("image %s", tag)
		return nil
	}
	if imageExists(tag) && !cfg.Rebuild {
		log.Printf("image hooks changed; rebuilding %s", tag)
	}
	return buildImage(tag, cfg.Version, arch, cfg.Root, hash)
}

func imageExists(tag string) bool {
	cmd := exec.Command("docker", "image", "inspect", tag)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func imageHash(tag string) string {
	out, err := exec.Command("docker", "image", "inspect",
		"-f", `{{index .Config.Labels "sfosbuild.image-hash"}}`, tag).Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "<no value>" {
		return ""
	}
	return s
}

func buildImage(tag, version, arch, root, hash string) error {
	platform := archPlatform[arch]
	ctx, err := os.MkdirTemp("", "sfosbuild-img-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ctx)
	scripts := imageScriptPaths(root)
	for _, f := range []string{"unpack.sh", "prepare.sh"} {
		data, err := embedded.ReadFile(f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(ctx, f), data, 0644); err != nil {
			return err
		}
	}
	dockerfile, err := embedded.ReadFile("Dockerfile")
	if err != nil {
		return err
	}
	if !strings.Contains(string(dockerfile), imageHooksMarker) {
		return fmt.Errorf("Dockerfile missing %q marker", strings.TrimSpace(imageHooksMarker))
	}
	dockerfile = []byte(strings.Replace(string(dockerfile), imageHooksMarker, imageHooksDockerfile(scripts), 1))
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), dockerfile, 0644); err != nil {
		return err
	}
	scriptDir := filepath.Join(ctx, "image-scripts")
	if err := os.MkdirAll(scriptDir, 0755); err != nil {
		return err
	}
	for _, s := range scripts {
		if err := cp(s, filepath.Join(scriptDir, filepath.Base(s))); err != nil {
			return err
		}
	}
	md5 := knownMD5[version+"/"+arch]
	log.Printf("building %s (downloads SDK target)", tag)
	args := dockerBuildArgs(tag, ctx, version, arch, platform, md5, hash)
	return runCmd(dockerCmd(args...))
}

func dockerBuildArgs(tag, context, version, arch, platform, md5, hash string) []string {
	return []string{
		"build",
		"--platform", platform,
		"--build-arg", "SFOS_VERSION=" + version,
		"--build-arg", "SFOS_ARCH=" + arch,
		"--build-arg", "SFOS_PLATFORM=" + platform,
		"--build-arg", "TARGET_7Z_MD5=" + md5,
		"--build-arg", "SFOSBUILD_IMAGE_HASH=" + hash,
		"-t", tag,
		context,
	}
}

func ensureBuildDepsImage(cfg *config, arch, pkgPath, meta, name, version string) (string, error) {
	base := imageTag(cfg.ImagePrefix, cfg.Version, arch)
	depsHash := buildDepsHash(meta, version)
	tag := imageBuildTag(cfg.ImagePrefix, cfg.Version, arch, depsHash)
	if !cfg.Rebuild && imageExists(tag) && imageBuildDepsHash(tag) == depsHash {
		log.Printf("image %s", tag)
		return tag, nil
	}
	if imageExists(tag) && !cfg.Rebuild {
		log.Printf("build requires changed; rebuilding %s", tag)
	}
	if err := buildDepsImage(tag, base, arch, pkgPath, meta, version, depsHash); err != nil {
		return "", err
	}
	return tag, nil
}

func imageBuildDepsHash(tag string) string {
	out, err := exec.Command("docker", "image", "inspect",
		"-f", `{{index .Config.Labels "sfosbuild.build-deps-hash"}}`, tag).Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "<no value>" {
		return ""
	}
	return s
}

func buildDepsImage(tag, base, arch, pkgPath, meta, version, depsHash string) error {
	ctx, err := os.MkdirTemp("", "sfosbuild-deps-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ctx)

	scriptBytes, err := embedded.ReadFile("install-builddeps.sh")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ctx, "install-builddeps.sh"), scriptBytes, 0644); err != nil {
		return err
	}
	pkgName := filepath.Base(pkgPath)
	if err := os.WriteFile(filepath.Join(ctx, pkgName), []byte(patchVersion(meta, version)), 0644); err != nil {
		return err
	}
	dockerfile := fmt.Sprintf(`FROM %s
ARG SFOSBUILD_DEPS_HASH=
COPY install-builddeps.sh /usr/bin/sfos-install-builddeps.sh
COPY %s /tmp/sfosbuild/%s
RUN echo "sfosbuild: installing build requires" && sh /usr/bin/sfos-install-builddeps.sh /tmp/sfosbuild/%s
LABEL sfosbuild.build-deps-hash=${SFOSBUILD_DEPS_HASH}
`, base, pkgName, pkgName, pkgName)
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		return err
	}

	platform := archPlatform[arch]
	log.Printf("building %s (build requires)", tag)
	args := []string{
		"build",
		"--platform", platform,
		"--build-arg", "SFOSBUILD_DEPS_HASH=" + depsHash,
		"-t", tag,
		ctx,
	}
	return runCmd(dockerCmd(args...))
}

func buildArch(cfg *config, arch, pkgPath, meta, name, version, out string) error {
	topdir, script, cleanup, err := prepareStage(cfg.Project, pkgPath, meta, name, version)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := ensureImage(cfg, arch); err != nil {
		return err
	}
	tag, err := ensureBuildDepsImage(cfg, arch, pkgPath, meta, name, version)
	if err != nil {
		return err
	}
	args := dockerRunArgs(tag, archPlatform[arch], arch, topdir, script, out)
	if err := runCmd(dockerCmd(args...)); err != nil {
		return fmt.Errorf("docker run: %w", err)
	}
	return nil
}

func dockerRunArgs(tag, platform, arch, topdir, script, out string) []string {
	args := []string{
		"run", "--rm",
		"--label", dockerRunLabel,
		"--platform", platform,
		"-e", "SFOS_ARCH=" + arch,
		"-e", "HOST_UID=" + strconv.Itoa(os.Getuid()),
		"-e", "HOST_GID=" + strconv.Itoa(os.Getgid()),
		"-v", topdir + ":/build",
		"-v", script + ":/usr/bin/sfos-rpmbuild.sh:ro",
		"-v", out + ":/out",
	}
	if v := os.Getenv("CERTS_VERSION"); v != "" {
		args = append(args, "-e", "CERTS_VERSION="+v)
	}
	args = append(args, tag, "sh", "/usr/bin/sfos-rpmbuild.sh")
	return args
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func remoteArch(userHost string) (string, error) {
	out, err := exec.Command("ssh", userHost, "uname -m").Output()
	if err != nil {
		return "", err
	}
	return normalizeArch(strings.TrimSpace(string(out)))
}

func remoteVersion(userHost string) (string, error) {
	out, err := exec.Command("ssh", userHost, "cat", "/etc/os-release").Output()
	if err != nil {
		return "", err
	}
	v := osReleaseField(string(out), "VERSION_ID")
	if v == "" {
		return "", fmt.Errorf("no VERSION_ID in /etc/os-release")
	}
	return v, nil
}

func findMainRPM(dir, pkgName string) (string, error) {
	if pkgName == "" {
		return "", fmt.Errorf("empty package name")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	prefix := pkgName + "-"
	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rpm") {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || strings.Contains(name, "debug") {
			continue
		}
		rest := strings.TrimSuffix(name, ".rpm")
		rest = rest[len(pkgName)+1:]
		for _, sub := range []string{"doc-", "tests-", "ts-devel-", "debuginfo", "debugsource"} {
			if strings.HasPrefix(rest, sub) {
				goto skip
			}
		}
		candidates = append(candidates, filepath.Join(dir, name))
	skip:
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no main rpm for %s in %s", pkgName, dir)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return len(candidates[i]) < len(candidates[j])
	})
	return candidates[0], nil
}

func installRPM(userHost, rpmPath string) error {
	base := filepath.Base(rpmPath)
	remote := "RPMS/" + base
	log.Printf("copying %s to %s:%s", base, userHost, remote)
	mkdir := exec.Command("ssh", userHost, "mkdir", "-p", "RPMS")
	if err := runCmd(mkdir); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	scp := exec.Command("scp", rpmPath, userHost+":"+remote)
	if err := runCmd(scp); err != nil {
		return fmt.Errorf("scp: %w", err)
	}
	log.Printf("installing on %s (confirm on device)", userHost)
	var install *exec.Cmd
	check := exec.Command("ssh", userHost, "command", "-v", "sdk-deploy-rpm")
	check.Stderr = io.Discard
	if check.Run() == nil {
		install = exec.Command("ssh", "-t", userHost, "sdk-deploy-rpm", remote)
	} else {
		install = exec.Command("ssh", "-t", userHost, "pkcon", "--plain", "--noninteractive", "install-local", remote)
	}
	install.Stdin = os.Stdin
	if err := runCmd(install); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	rm := exec.Command("ssh", userHost, "rm", "-f", remote)
	rm.Run()
	return nil
}

func dockerCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	return cmd
}

func runCmd(cmd *exec.Cmd) error {
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.Start(); err != nil {
		return err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		return err
	case <-ctx.Done():
		killSfosbuildDocker()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-waitCh
		return fmt.Errorf("interrupted")
	}
}

func killSfosbuildDocker() {
	out, err := exec.Command("docker", "ps", "-q", "--filter", "label="+dockerRunLabel).Output()
	if err != nil {
		return
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) == 0 {
		return
	}
	exec.Command("docker", append([]string{"kill"}, ids...)...).Run()
}
