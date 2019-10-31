package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

// CleanPath makes a path safe for use with filepath.Join. This is done by not
// only cleaning the path, but also (if the path is relative) adding a leading
// '/' and cleaning it (then removing the leading '/'). This ensures that a
// path resulting from prepending another path will always resolve to lexically
// be a subdirectory of the prefixed path. This is all done lexically, so paths
// that include symlinks won't be safe as a result of using CleanPath.
func CleanPath(path string) string {
	// Deal with empty strings nicely.
	if path == "" {
		return ""
	}

	// Ensure that all paths are cleaned (especially problematic ones like
	// "/../../../../../" which can cause lots of issues).
	path = filepath.Clean(path)

	// If the path isn't absolute, we need to do more processing to fix paths
	// such as "../../../../<etc>/some/path". We also shouldn't convert absolute
	// paths to relative ones.
	if !filepath.IsAbs(path) {
		path = filepath.Clean(string(os.PathSeparator) + path)
		// This can't fail, as (by definition) all paths are relative to root.
		path, _ = filepath.Rel(string(os.PathSeparator), path)
	}

	// Clean the path again for good measure.
	return filepath.Clean(path)
}

// FollowSymlinkInScope is a wrapper around evalSymlinksInScope that returns an absolute path
func FollowSymlinkInScope(path, root string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return evalSymlinksInScope(path, root)
}

// evalSymlinksInScope will evaluate symlinks in `path` within a scope `root` and return
// a result guaranteed to be contained within the scope `root`, at the time of the call.
// Symlinks in `root` are not evaluated and left as-is.
// Errors encountered while attempting to evaluate symlinks in path will be returned.
// Non-existing paths are valid and do not constitute an error.
// `path` has to contain `root` as a prefix, or else an error will be returned.
// Trying to break out from `root` does not constitute an error.
//
// Example:
//   If /foo/bar -> /outside,
//   FollowSymlinkInScope("/foo/bar", "/foo") == "/foo/outside" instead of "/oustide"
//
// IMPORTANT: it is the caller's responsibility to call evalSymlinksInScope *after* relevant symlinks
// are created and not to create subsequently, additional symlinks that could potentially make a
// previously-safe path, unsafe. Example: if /foo/bar does not exist, evalSymlinksInScope("/foo/bar", "/foo")
// would return "/foo/bar". If one makes /foo/bar a symlink to /baz subsequently, then "/foo/bar" should
// no longer be considered safely contained in "/foo".
func evalSymlinksInScope(path, root string) (string, error) {
	root = filepath.Clean(root)
	if path == root {
		return path, nil
	}
	if !strings.HasPrefix(path, root) {
		return "", errors.New("evalSymlinksInScope: " + path + " is not in " + root)
	}
	const maxIter = 255
	originalPath := path
	// given root of "/a" and path of "/a/b/../../c" we want path to be "/b/../../c"
	path = path[len(root):]
	if root == string(filepath.Separator) {
		path = string(filepath.Separator) + path
	}
	if !strings.HasPrefix(path, string(filepath.Separator)) {
		return "", errors.New("evalSymlinksInScope: " + path + " is not in " + root)
	}
	path = filepath.Clean(path)
	// consume path by taking each frontmost path element,
	// expanding it if it's a symlink, and appending it to b
	var b bytes.Buffer
	// b here will always be considered to be the "current absolute path inside
	// root" when we append paths to it, we also append a slash and use
	// filepath.Clean after the loop to trim the trailing slash
	for n := 0; path != ""; n++ {
		if n > maxIter {
			return "", errors.New("evalSymlinksInScope: too many links in " + originalPath)
		}

		// find next path component, p
		i := strings.IndexRune(path, filepath.Separator)
		var p string
		if i == -1 {
			p, path = path, ""
		} else {
			p, path = path[:i], path[i+1:]
		}

		if p == "" {
			continue
		}

		// this takes a b.String() like "b/../" and a p like "c" and turns it
		// into "/b/../c" which then gets filepath.Cleaned into "/c" and then
		// root gets prepended and we Clean again (to remove any trailing slash
		// if the first Clean gave us just "/")
		cleanP := filepath.Clean(string(filepath.Separator) + b.String() + p)
		if cleanP == string(filepath.Separator) {
			// never Lstat "/" itself
			b.Reset()
			continue
		}
		fullP := filepath.Clean(root + cleanP)

		fi, err := os.Lstat(fullP)
		if os.IsNotExist(err) {
			// if p does not exist, accept it
			b.WriteString(p)
			b.WriteRune(filepath.Separator)
			continue
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			b.WriteString(p + string(filepath.Separator))
			continue
		}

		// it's a symlink, put it at the front of path
		dest, err := os.Readlink(fullP)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(dest) {
			b.Reset()
		}
		path = dest + string(filepath.Separator) + path
	}

	// see note above on "fullP := ..." for why this is double-cleaned and
	// what's happening here
	return filepath.Clean(root + filepath.Clean(string(filepath.Separator)+b.String())), nil
}

func prepareConfig(spec *specs.Spec) (*configs.Config, error) {
	rcwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cwd, err := filepath.Abs(rcwd)
	if err != nil {
		return nil, err
	}
	if spec.Root == nil {
		return nil, fmt.Errorf("root must be specified")
	}
	rootfsPath := spec.Root.Path
	if !filepath.IsAbs(rootfsPath) {
		rootfsPath = filepath.Join(cwd, rootfsPath)
	}
	labels := []string{}
	for k, v := range spec.Annotations {
		labels = append(labels, fmt.Sprintf("%s=%s", k, v))
	}
	config := &configs.Config{
		Rootfs:      rootfsPath,
		NoPivotRoot: false,
		Readonlyfs:  spec.Root.Readonly,
		Hostname:    spec.Hostname,
		Labels:      append(labels, fmt.Sprintf("bundle=%s", cwd)),
		//NoNewKeyring: false,
		//Rootless:     false,
	}

	for _, m := range spec.Mounts {
		config.Mounts = append(config.Mounts, createLibcontainerMount(cwd, m))
	}
	return config, nil
}

func validateProcessSpec(spec *specs.Process) error {
	if spec.Cwd == "" {
		return fmt.Errorf("cwd property must not be empty")
	}
	if !filepath.IsAbs(spec.Cwd) {
		return fmt.Errorf("cwd must be an absolute path")
	}
	if len(spec.Args) == 0 {
		return fmt.Errorf("args must not be empty")
	}
	return nil
}

func initSpec(sepcConf string) (*specs.Spec, error) {
	rcwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cwd, err := filepath.Abs(rcwd)
	if err != nil {
		return nil, err
	}
	specPath := filepath.Join(cwd, sepcConf)
	cf, err := os.Open(specPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("JSON specification file %s not found", specPath)
		}
		return nil, err
	}
	defer cf.Close()

	var spec = new(specs.Spec)
	if err = json.NewDecoder(cf).Decode(&spec); err != nil {
		return nil, err
	}
	return spec, validateProcessSpec(spec.Process)
}

func IsInStringArray(val string, array []string) bool {
	for _, ele := range array {
		if ele == val {
			return true
		}
	}
	return false
}

func FilePutContents(filename string, content string, modAppend bool) error {
	var mode = os.O_WRONLY | os.O_CREATE
	if modAppend {
		mode = mode | os.O_APPEND
	} else {
		mode = mode | os.O_TRUNC
	}
	fd, err := os.OpenFile(filename, mode, 0644)
	if err != nil {
		return err
	}
	defer fd.Close()
	_, err = fd.WriteString(content)
	return err
}

func FileGetContents(file string) (string, error) {
	content, err := ioutil.ReadFile(file)
	if err != nil {
		return "", err
	}
	return string(content), nil
}
