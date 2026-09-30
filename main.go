package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	InPlace     bool // working tree via rpmbuild --build-in-place; otherwise git archive of HEAD and local submodules
	Source      bool // rpmbuild -ba and copy SRPMS into the output directory
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

	var sourceTar string
	if !cfg.InPlace {
		tarPath, tarCleanup, err := writeGitArchive(cfg.Project, name, version)
		if err != nil {
			return err
		}
		defer tarCleanup()
		sourceTar = tarPath
		log.Printf("source %s", filepath.Base(sourceTar))
	}

	for _, arch := range cfg.Arches {
		log.Printf("%s %s %s", cfg.Version, arch, name)
		if err := buildArch(cfg, arch, pkgPath, string(meta), name, version, outAbs, sourceTar); err != nil {
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
	cfg.InPlace = true
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
	pkgVersion := os.Getenv("RPM_VERSION")
	if pkgVersion == "" {
		pkgVersion = specField(string(meta), "Version")
	}
	if pkgVersion == "" {
		return fmt.Errorf("no Version: in %s (or RPM_VERSION)", pkgPath)
	}
	rpms, err := findDeployRPMs(outAbs, pkgName, pkgVersion, arch)
	if err != nil {
		return err
	}
	for _, rpm := range rpms {
		if err := installRPM(userHost, rpm); err != nil {
			return err
		}
	}
	return nil
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
  $wd/.sfosbuild/image/, and shell mounts $wd at the same path. Host $HOME
  is mounted read-only at the same path, and container /root is bound to
  ~/.cache/sfosbuild/<version>/<arch>/m_root, on every docker run (build
  and shell).

  .sfosbuild/image/*.sh
      Run during SDK image build, in sorted order, after the target is
      unpacked. The image is rebuilt when these scripts change (or with
      --rebuild).

Commands:
  build (default)  Package a project with specify + rpmbuild
  deploy           ssh uname -m and /etc/os-release, build, install the RPMs for that architecture
                   device ~/.sfosbuild-os-version (x.x.x.x) overrides the probed OS version
  shell            docker run -i -t -v $HOME:$HOME:ro -v $wd:$wd -v m_root:/root -w $PWD in the SDK image
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
  --in-place           rpmbuild --build-in-place on the working tree
  --source             also build the .src.rpm and copy SRPMS to the output directory
  --rebuild            rebuild SDK images even if they exist
  --image-prefix NAME  docker image prefix (default: sfosbuild)
  -h, --help           show this help

Architectures: aarch64, armv7hl (armv7a), i486, all
  shell takes a single arch (not all).

The project needs rpm/*.yaml (spectacle). specify runs in the SDK image
to generate the spec. build packages the git HEAD tree as Source0,
including submodules already present in the local repository, and runs a
clean rpmbuild. That container runs with --network=none. SDK image setup
and build-requires install still use the network. --in-place and deploy use
rpmbuild --build-in-place on the working tree. Drop the previous arch's
build tree first (make clean and make distclean) so that cache is not packaged.
Images are tagged <prefix>_<workspace>:<version>-<arch>, where
<workspace> is the directory that contains .sfosbuild/.
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
		case a == "--in-place" || a == "-in-place":
			cfg.InPlace = true
		case a == "--source":
			cfg.Source = true
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
	tag := imageTag(cfg.ImagePrefix, cfg.Root, cfg.Version, arch)
	merRoot, err := ensureMerRoot(cfg.Version, arch)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dargs := dockerShellArgs(tag, archPlatform[arch], arch, cfg.Root, cfg.WorkDir, merRoot, home, cmd, len(cmd) == 0)
	c := dockerCmd(dargs...)
	c.Stdin = os.Stdin
	return runCmd(c)
}

func dockerShellArgs(tag, platform, arch, wd, workdir, merRoot, home string, cmd []string, tty bool) []string {
	args := []string{"run", "--rm", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args,
		"--label", dockerRunLabel,
		"--platform", platform,
		"-e", "SFOS_ARCH="+arch,
		"-v", home+":"+home+":ro",
		"-v", wd+":"+wd,
		"-v", merRoot+":/root",
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

var imageNameInvalid = regexp.MustCompile(`[^a-z0-9._-]+`)

func imageRepo(prefix, root string) string {
	return prefix + "_" + sanitizeImageName(filepath.Base(root))
}

func sanitizeImageName(s string) string {
	s = strings.Trim(imageNameInvalid.ReplaceAllString(strings.ToLower(s), "-"), "-._")
	if s == "" {
		return "workspace"
	}
	return s
}

func imageTag(prefix, root, version, arch string) string {
	return imageRepo(prefix, root) + ":" + version + "-" + arch
}

func imageBuildTag(prefix, root, version, arch, depsHash string) string {
	return fmt.Sprintf("%s:%s-%s-%s", imageRepo(prefix, root), version, arch, depsHash)
}

func buildDepsHash(meta, version, baseID string) string {
	h := sha256.New()
	h.Write([]byte(patchVersion(meta, version)))
	h.Write([]byte(baseID))
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

func writeGitArchive(project, name, version string) (path string, cleanup func(), err error) {
	top, err := gitOutput(project, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, fmt.Errorf("%s is not a git repository (%w)", project, err)
	}
	prefix, err := gitOutput(project, "rev-parse", "--show-prefix")
	if err != nil {
		return "", nil, err
	}
	if dirty, err := gitOutput(project, "status", "--porcelain"); err == nil && strings.TrimSpace(dirty) != "" {
		log.Printf("working tree has uncommitted changes; build uses HEAD")
	}
	work, err := os.MkdirTemp("", "sfosbuild-src-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(work) }
	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", nil, err
	}
	tree := "HEAD"
	if prefix != "" {
		tree = "HEAD:" + strings.TrimSuffix(prefix, "/")
	}
	tarPath := filepath.Join(work, name+"-"+version+".tar")
	tf, err := os.Create(tarPath)
	if err != nil {
		return fail(err)
	}
	tw := tar.NewWriter(tf)
	archErr := archiveTree(tw, top, "", tree, name+"-"+version+"/", map[string]bool{})
	closeErr := tw.Close()
	fileErr := tf.Close()
	if archErr != nil {
		return fail(archErr)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	if fileErr != nil {
		return fail(fileErr)
	}
	path = filepath.Join(work, name+"-"+version+".tar.bz2")
	if err := bzip2File(tarPath, path); err != nil {
		return fail(err)
	}
	return path, cleanup, nil
}

// archiveTree packs tree and any gitlinks it records. Submodule contents come
// from the local module git dir (.git/modules/...). Nothing is fetched or
// checked out.
func archiveTree(tw *tar.Writer, workTree, gitDir, tree, prefix string, seen map[string]bool) error {
	key := workTree + "\x00" + gitDir + "\x00" + tree
	if seen[key] {
		return fmt.Errorf("submodule cycle at %s", tree)
	}
	seen[key] = true
	if err := copyGitArchive(tw, workTree, gitDir, tree, prefix); err != nil {
		return err
	}
	return appendSubmodules(tw, workTree, gitDir, tree, prefix, seen)
}

func copyGitArchive(tw *tar.Writer, workTree, gitDir, tree, prefix string) error {
	cmd := gitCmd(workTree, gitDir, "archive", "--format=tar", "--prefix", prefix, tree)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	copyErr := copyTar(tw, stdout)
	waitErr := cmd.Wait()
	if copyErr != nil {
		return copyErr
	}
	if waitErr != nil {
		return fmt.Errorf("git archive: %w", waitErr)
	}
	return nil
}

func copyTar(tw *tar.Writer, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

type gitlink struct {
	sha  string
	path string
}

func appendSubmodules(tw *tar.Writer, workTree, gitDir, tree, prefix string, seen map[string]bool) error {
	links, err := gitlinks(workTree, gitDir, tree)
	if err != nil || len(links) == 0 {
		return err
	}
	commit, subdir := tree, ""
	if i := strings.Index(tree, ":"); i >= 0 {
		commit, subdir = tree[:i], tree[i+1:]
	}
	names, err := submoduleNames(workTree, gitDir, commit)
	if err != nil {
		return err
	}
	for _, link := range links {
		full := link.path
		if subdir != "" {
			full = subdir + "/" + link.path
		}
		name, ok := names[full]
		if !ok {
			return fmt.Errorf("gitlink %s has no .gitmodules entry", full)
		}
		subGit, err := submoduleGitDir(workTree, gitDir, name)
		if err != nil {
			return err
		}
		if _, err := gitOutputOpt("", subGit, "cat-file", "-e", link.sha+"^{commit}"); err != nil {
			return fmt.Errorf("submodule %s commit %s is not present locally", full, link.sha)
		}
		log.Printf("submodule %s", full)
		if err := archiveTree(tw, "", subGit, link.sha, prefix+link.path+"/", seen); err != nil {
			return err
		}
	}
	return nil
}

func gitlinks(workTree, gitDir, tree string) ([]gitlink, error) {
	out, err := gitOutputBytes(workTree, gitDir, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	var links []gitlink
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(string(rec[:tab]))
		if len(fields) < 3 || fields[0] != "160000" {
			continue
		}
		links = append(links, gitlink{sha: fields[2], path: string(rec[tab+1:])})
	}
	return links, nil
}

func submoduleNames(workTree, gitDir, commit string) (map[string]string, error) {
	if _, err := gitOutputOpt(workTree, gitDir, "cat-file", "-e", commit+":.gitmodules"); err != nil {
		return map[string]string{}, nil
	}
	out, err := gitOutputBytes(workTree, gitDir, "show", commit+":.gitmodules")
	if err != nil {
		return nil, err
	}
	return parseGitmodules(string(out)), nil
}

func parseGitmodules(data string) map[string]string {
	pathToName := map[string]string{}
	var name, path string
	flush := func() {
		if name != "" && path != "" {
			pathToName[path] = name
		}
	}
	for _, line := range strings.Split(data, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, `[submodule "`) && strings.HasSuffix(trim, `"]`) {
			flush()
			name = trim[len(`[submodule "`) : len(trim)-2]
			path = ""
			continue
		}
		key, val, ok := strings.Cut(trim, "=")
		if !ok || strings.TrimSpace(key) != "path" {
			continue
		}
		path = strings.TrimSpace(val)
	}
	flush()
	return pathToName
}

func submoduleGitDir(workTree, gitDir, name string) (string, error) {
	out, err := gitOutputOpt(workTree, gitDir, "rev-parse", "--path-format=absolute", "--git-path", "modules/"+name)
	if err == nil {
		return out, nil
	}
	out, err = gitOutputOpt(workTree, gitDir, "rev-parse", "--git-path", "modules/"+name)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(out) {
		return out, nil
	}
	base := workTree
	if base == "" {
		var cwdErr error
		base, cwdErr = os.Getwd()
		if cwdErr != nil {
			return "", cwdErr
		}
	}
	return filepath.Join(base, out), nil
}

func bzip2File(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bzip2: %w", err)
	}
	return nil
}

func gitCmd(workTree, gitDir string, args ...string) *exec.Cmd {
	full := make([]string, 0, len(args)+4)
	if gitDir != "" {
		full = append(full, "--git-dir", gitDir)
	}
	if workTree != "" {
		full = append(full, "-C", workTree)
	}
	full = append(full, args...)
	return exec.Command("git", full...)
}

func gitOutputOpt(workTree, gitDir string, args ...string) (string, error) {
	out, err := gitOutputBytes(workTree, gitDir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitOutputBytes(workTree, gitDir string, args ...string) ([]byte, error) {
	cmd := gitCmd(workTree, gitDir, args...)
	return cmd.Output()
}

func gitOutput(dir string, args ...string) (string, error) {
	return gitOutputOpt(dir, "", args...)
}

func prepareStage(pkgPath, meta, name, version, sourceTar string) (topdir, script string, cleanup func(), err error) {
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
	if sourceTar != "" {
		dst := filepath.Join(topdir, "SOURCES", filepath.Base(sourceTar))
		if err := cp(sourceTar, dst); err != nil {
			cleanup()
			return "", "", nil, err
		}
	}
	return topdir, script, cleanup, nil
}

func ensureImage(cfg *config, arch string) error {
	tag := imageTag(cfg.ImagePrefix, cfg.Root, cfg.Version, arch)
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

func imageID(tag string) (string, error) {
	out, err := exec.Command("docker", "image", "inspect", "-f", "{{.Id}}", tag).Output()
	if err != nil {
		return "", fmt.Errorf("image %s: %w", tag, err)
	}
	return strings.TrimSpace(string(out)), nil
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
	base := imageTag(cfg.ImagePrefix, cfg.Root, cfg.Version, arch)
	baseID, err := imageID(base)
	if err != nil {
		return "", err
	}
	depsHash := buildDepsHash(meta, version, baseID)
	tag := imageBuildTag(cfg.ImagePrefix, cfg.Root, cfg.Version, arch, depsHash)
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

func buildArch(cfg *config, arch, pkgPath, meta, name, version, out, sourceTar string) error {
	topdir, script, cleanup, err := prepareStage(pkgPath, meta, name, version, sourceTar)
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
	merRoot, err := ensureMerRoot(cfg.Version, arch)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	args := dockerRunArgs(tag, archPlatform[arch], arch, cfg.Project, topdir, script, out, merRoot, home, cfg.InPlace, cfg.Source)
	if err := runCmd(dockerCmd(args...)); err != nil {
		return fmt.Errorf("docker run: %w", err)
	}
	return nil
}

func dockerRunArgs(tag, platform, arch, project, topdir, script, out, merRoot, home string, inPlace, source bool) []string {
	workdir := "/rpmbuild"
	args := []string{
		"run", "--rm",
		"--network", "none",
		"--label", dockerRunLabel,
		"--platform", platform,
		"-e", "SFOS_ARCH=" + arch,
		"-e", "HOST_UID=" + strconv.Itoa(os.Getuid()),
		"-e", "HOST_GID=" + strconv.Itoa(os.Getgid()),
		"-v", home + ":" + home + ":ro",
	}
	if inPlace {
		workdir = "/build"
		args = append(args,
			"-e", "SFOS_INPLACE=1",
			"-v", project+":/build",
		)
	}
	if source {
		args = append(args, "-e", "SFOS_SOURCE=1")
	}
	args = append(args,
		"-v", topdir+":/rpmbuild",
		"-w", workdir,
		"-v", script+":/usr/bin/sfos-rpmbuild.sh:ro",
		"-v", out+":/out",
		"-v", merRoot+":/root",
	)
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

func cacheDir() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".cache")
	}
	return filepath.Join(home, ".cache")
}

func merRootDir(version, arch string) string {
	return filepath.Join(cacheDir(), "sfosbuild", version, arch, "m_root")
}

func ensureMerRoot(version, arch string) (string, error) {
	dir := merRootDir(version, arch)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

func remoteArch(userHost string) (string, error) {
	out, err := exec.Command("ssh", userHost, "uname -m").Output()
	if err != nil {
		return "", err
	}
	return normalizeArch(strings.TrimSpace(string(out)))
}

func validOSVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func versionFromProbe(override, osRelease string) string {
	if v := strings.TrimSpace(override); validOSVersion(v) {
		return v
	}
	return osReleaseField(osRelease, "VERSION_ID")
}

func remoteVersion(userHost string) (string, error) {
	override, _ := exec.Command("ssh", userHost, `cat "$HOME/.sfosbuild-os-version"`).Output()
	if v := versionFromProbe(string(override), ""); v != "" {
		return v, nil
	}
	out, err := exec.Command("ssh", userHost, "cat", "/etc/os-release").Output()
	if err != nil {
		return "", err
	}
	v := versionFromProbe("", string(out))
	if v == "" {
		return "", fmt.Errorf("no VERSION_ID in /etc/os-release")
	}
	return v, nil
}

// parseRPMFilename splits an rpmbuild filename, N-V-R.A.rpm. Name may contain hyphens.
func parseRPMFilename(filename string) (name, version, release, arch string, ok bool) {
	base := strings.TrimSuffix(filename, ".rpm")
	if base == filename || base == "" {
		return
	}
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return
	}
	arch = base[dot+1:]
	nvr := base[:dot]
	relDash := strings.LastIndex(nvr, "-")
	if relDash <= 0 {
		return
	}
	release = nvr[relDash+1:]
	nv := nvr[:relDash]
	verDash := strings.LastIndex(nv, "-")
	if verDash <= 0 {
		return
	}
	version = nv[verDash+1:]
	name = nv[:verDash]
	ok = name != "" && version != "" && release != "" && arch != ""
	return
}

type rpmFile struct {
	path    string
	name    string
	release string
	arch    string
	mod     time.Time
}

// findDeployRPMs returns the RPMs this build produced for arch: the spec
// package, its subpackages, and noarch packages of the same version.
// Other architectures and older releases left in the output directory are
// skipped. The main package is first so subpackages can depend on it.
func findDeployRPMs(dir, pkgName, version, arch string) ([]string, error) {
	if pkgName == "" {
		return nil, fmt.Errorf("empty package name")
	}
	if version == "" {
		return nil, fmt.Errorf("empty version")
	}
	if arch == "" {
		return nil, fmt.Errorf("empty arch")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var matched []rpmFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ver, rel, rpmArch, ok := parseRPMFilename(e.Name())
		if !ok || ver != version {
			continue
		}
		if name != pkgName && !strings.HasPrefix(name, pkgName+"-") {
			continue
		}
		if rpmArch != arch && rpmArch != "noarch" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		matched = append(matched, rpmFile{
			path:    filepath.Join(dir, e.Name()),
			name:    name,
			release: rel,
			arch:    rpmArch,
			mod:     info.ModTime(),
		})
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("no rpms for %s-%s (%s) in %s", pkgName, version, arch, dir)
	}
	newest := matched[0]
	for _, f := range matched[1:] {
		if f.mod.After(newest.mod) {
			newest = f
		}
	}
	hasArch := map[string]bool{}
	for _, f := range matched {
		if f.release == newest.release && f.arch == arch {
			hasArch[f.name] = true
		}
	}
	var chosen []rpmFile
	for _, f := range matched {
		if f.release != newest.release {
			continue
		}
		if f.arch == "noarch" && hasArch[f.name] {
			continue
		}
		chosen = append(chosen, f)
	}
	sort.Slice(chosen, func(i, j int) bool {
		ri, rj := deployRank(pkgName, arch, chosen[i]), deployRank(pkgName, arch, chosen[j])
		if ri != rj {
			return ri < rj
		}
		return chosen[i].path < chosen[j].path
	})
	out := make([]string, len(chosen))
	for i, f := range chosen {
		out[i] = f.path
	}
	return out, nil
}

func deployRank(pkgName, arch string, f rpmFile) int {
	switch {
	case f.name == pkgName && f.arch == arch:
		return 0
	case f.name == pkgName:
		return 1
	case strings.HasSuffix(f.name, "-debuginfo"), strings.HasSuffix(f.name, "-debugsource"):
		return 3
	default:
		return 2
	}
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
