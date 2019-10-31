package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/pkg/mount"
	"github.com/mrunalp/fileutils"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

const defaultMountFlags = unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV

// parseMountOptions parses the string and returns the flags, propagation
// flags and any mount data that it contains.
func parseMountOptions(options []string) (int, []int, string, int) {
	var (
		flag     int
		pgflag   []int
		data     []string
		extFlags int
	)
	flags := map[string]struct {
		clear bool
		flag  int
	}{
		"acl":           {false, unix.MS_POSIXACL},
		"async":         {true, unix.MS_SYNCHRONOUS},
		"atime":         {true, unix.MS_NOATIME},
		"bind":          {false, unix.MS_BIND},
		"defaults":      {false, 0},
		"dev":           {true, unix.MS_NODEV},
		"diratime":      {true, unix.MS_NODIRATIME},
		"dirsync":       {false, unix.MS_DIRSYNC},
		"exec":          {true, unix.MS_NOEXEC},
		"iversion":      {false, unix.MS_I_VERSION},
		"lazytime":      {false, unix.MS_LAZYTIME},
		"loud":          {true, unix.MS_SILENT},
		"mand":          {false, unix.MS_MANDLOCK},
		"noacl":         {true, unix.MS_POSIXACL},
		"noatime":       {false, unix.MS_NOATIME},
		"nodev":         {false, unix.MS_NODEV},
		"nodiratime":    {false, unix.MS_NODIRATIME},
		"noexec":        {false, unix.MS_NOEXEC},
		"noiversion":    {true, unix.MS_I_VERSION},
		"nolazytime":    {true, unix.MS_LAZYTIME},
		"nomand":        {true, unix.MS_MANDLOCK},
		"norelatime":    {true, unix.MS_RELATIME},
		"nostrictatime": {true, unix.MS_STRICTATIME},
		"nosuid":        {false, unix.MS_NOSUID},
		"rbind":         {false, unix.MS_BIND | unix.MS_REC},
		"relatime":      {false, unix.MS_RELATIME},
		"remount":       {false, unix.MS_REMOUNT},
		"ro":            {false, unix.MS_RDONLY},
		"rw":            {true, unix.MS_RDONLY},
		"silent":        {false, unix.MS_SILENT},
		"strictatime":   {false, unix.MS_STRICTATIME},
		"suid":          {true, unix.MS_NOSUID},
		"sync":          {false, unix.MS_SYNCHRONOUS},
	}
	propagationFlags := map[string]int{
		"private":     unix.MS_PRIVATE,
		"shared":      unix.MS_SHARED,
		"slave":       unix.MS_SLAVE,
		"unbindable":  unix.MS_UNBINDABLE,
		"rprivate":    unix.MS_PRIVATE | unix.MS_REC,
		"rshared":     unix.MS_SHARED | unix.MS_REC,
		"rslave":      unix.MS_SLAVE | unix.MS_REC,
		"runbindable": unix.MS_UNBINDABLE | unix.MS_REC,
	}
	extensionFlags := map[string]struct {
		clear bool
		flag  int
	}{
		"tmpcopyup": {false, 1},
	}
	for _, o := range options {
		// If the option does not exist in the flags table or the flag
		// is not supported on the platform,
		// then it is a data value for a specific fs type
		if f, exists := flags[o]; exists && f.flag != 0 {
			if f.clear {
				flag &= ^f.flag
			} else {
				flag |= f.flag
			}
		} else if f, exists := propagationFlags[o]; exists && f != 0 {
			pgflag = append(pgflag, f)
		} else if f, exists := extensionFlags[o]; exists && f.flag != 0 {
			if f.clear {
				extFlags &= ^f.flag
			} else {
				extFlags |= f.flag
			}
		} else {
			data = append(data, o)
		}
	}
	return flag, pgflag, strings.Join(data, ","), extFlags
}

func createLibcontainerMount(cwd string, m specs.Mount) *configs.Mount {
	flags, pgflags, data, _ := parseMountOptions(m.Options)
	source := m.Source
	if m.Type == "bind" {
		if !filepath.IsAbs(source) {
			source = filepath.Join(cwd, m.Source)
		}
	}
	return &configs.Mount{
		Device:           m.Type,
		Source:           source,
		Destination:      m.Destination,
		Data:             data,
		Flags:            flags,
		PropagationFlags: pgflags,
		// Extensions:       ext,
	}
}

// FormatMountLabel returns a string to be used by the mount command.
// The format of this string will be used to alter the labeling of the mountpoint.
// The string returned is suitable to be used as the options field of the mount command.
// If you need to have additional mount point options, you can pass them in as
// the first parameter.  Second parameter is the label that you wish to apply
// to all content in the mount point.
func FormatMountLabel(src, mountLabel string) string {
	if mountLabel != "" {
		switch src {
		case "":
			src = fmt.Sprintf("context=%q", mountLabel)
		default:
			src = fmt.Sprintf("%s,context=%q", src, mountLabel)
		}
	}
	return src
}

// Do the mount operation followed by additional mounts required to take care
// of propagation flags.
func mountPropagate(m *configs.Mount, rootfs string, mountLabel string) error {
	var (
		dest  = m.Destination
		data  = FormatMountLabel(m.Data, mountLabel)
		flags = m.Flags
	)
	if CleanPath(dest) == "/dev" {
		flags &= ^unix.MS_RDONLY
	}

	// copyUp := m.Extensions&1 == 1
	var copyUp = false
	if !(copyUp || strings.HasPrefix(dest, rootfs)) {
		dest = filepath.Join(rootfs, dest)
	}

	if err := unix.Mount(m.Source, dest, m.Device, uintptr(flags), data); err != nil {
		return err
	}

	for _, pflag := range m.PropagationFlags {
		if err := unix.Mount("", dest, "", uintptr(pflag), ""); err != nil {
			return err
		}
	}
	return nil
}

// checkMountDestination checks to ensure that the mount destination is not over the top of /proc.
// dest is required to be an abs path and have any symlinks resolved before calling this function.
func checkMountDestination(rootfs, dest string) error {
	invalidDestinations := []string{
		"/proc",
	}
	// White list, it should be sub directories of invalid destinations
	validDestinations := []string{
		// These entries can be bind mounted by files emulated by fuse,
		// so commands like top, free displays stats in container.
		"/proc/cpuinfo",
		"/proc/diskstats",
		"/proc/meminfo",
		"/proc/stat",
		"/proc/swaps",
		"/proc/uptime",
		"/proc/net/dev",
	}
	for _, valid := range validDestinations {
		path, err := filepath.Rel(filepath.Join(rootfs, valid), dest)
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
	}
	for _, invalid := range invalidDestinations {
		path, err := filepath.Rel(filepath.Join(rootfs, invalid), dest)
		if err != nil {
			return err
		}
		if path == "." || !strings.HasPrefix(path, "..") {
			return fmt.Errorf("%q cannot be mounted because it is located inside %q", dest, invalid)
		}
	}
	return nil
}

// createIfNotExists creates a file or a directory only if it does not already exist.
func createIfNotExists(path string, isDir bool) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			if isDir {
				return os.MkdirAll(path, 0755)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE, 0755)
			if err != nil {
				return err
			}
			f.Close()
		}
	}
	return nil
}

func mountToRootfs(m *configs.Mount, rootfs, mountLabel string) error {
	var (
		dest = m.Destination
	)
	if !strings.HasPrefix(dest, rootfs) {
		dest = filepath.Join(rootfs, dest)
	}

	switch m.Device {
	case "proc", "sysfs":
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		// Selinux kernels do not support labeling of /proc or /sys
		return mountPropagate(m, rootfs, "")
	// case "mqueue":
	//  if err := os.MkdirAll(dest, 0755); err != nil {
	//      return err
	//  }
	//  if err := mountPropagate(m, rootfs, mountLabel); err != nil {
	//      // older kernels do not support labeling of /dev/mqueue
	//      if err := mountPropagate(m, rootfs, ""); err != nil {
	//          return err
	//      }
	//      return label.SetFileLabel(dest, mountLabel)
	//  }
	//  return nil
	case "tmpfs":
		var copyUp = false
		// copyUp := m.Extensions&1 == 1
		tmpDir := ""
		stat, err := os.Stat(dest)
		if err != nil {
			if err := os.MkdirAll(dest, 0755); err != nil {
				return err
			}
		}
		if copyUp {
			tmpDir, err = ioutil.TempDir("/tmp", "runctmpdir")
			if err != nil {
				return errors.Wrap(err, "tmpcopyup: failed to create tmpdir")
			}
			defer os.RemoveAll(tmpDir)
			m.Destination = tmpDir
		}
		if err := mountPropagate(m, rootfs, mountLabel); err != nil {
			return err
		}
		if copyUp {
			if err := fileutils.CopyDirectory(dest, tmpDir); err != nil {
				errMsg := fmt.Errorf("tmpcopyup: failed to copy %s to %s: %v", dest, tmpDir, err)
				if err1 := unix.Unmount(tmpDir, unix.MNT_DETACH); err1 != nil {
					return errors.Wrapf(err1, "tmpcopyup: %v: failed to unmount", errMsg)
				}
				return errMsg
			}
			if err := unix.Mount(tmpDir, dest, "", unix.MS_MOVE, ""); err != nil {
				errMsg := fmt.Errorf("tmpcopyup: failed to move mount %s to %s: %v", tmpDir, dest, err)
				if err1 := unix.Unmount(tmpDir, unix.MNT_DETACH); err1 != nil {
					return errors.Wrapf(err1, "tmpcopyup: %v: failed to unmount", errMsg)
				}
				return errMsg
			}
		}
		if stat != nil {
			if err = os.Chmod(dest, stat.Mode()); err != nil {
				return err
			}
		}
		return nil
	case "bind":
		stat, err := os.Stat(m.Source)
		if err != nil {
			// error out if the source of a bind mount does not exist as we will be
			// unable to bind anything to it.
			return err
		}
		// ensure that the destination of the bind mount is resolved of symlinks at mount time because
		// any previous mounts can invalidate the next mount's destination.
		// this can happen when a user specifies mounts within other mounts to cause breakouts or other
		// evil stuff to try to escape the container's rootfs.
		if dest, err = FollowSymlinkInScope(dest, rootfs); err != nil {
			return err
		}
		if err := checkMountDestination(rootfs, dest); err != nil {
			return err
		}
		// update the mount with the correct dest after symlinks are resolved.
		m.Destination = dest
		if err := createIfNotExists(dest, stat.IsDir()); err != nil {
			return err
		}
		if err := mountPropagate(m, rootfs, mountLabel); err != nil {
			return err
		}
		// bind mount won't change mount options, we need remount to make mount options effective.
		// first check that we have non-default options required before attempting a remount
		if m.Flags&^(unix.MS_REC|unix.MS_REMOUNT|unix.MS_BIND) != 0 {
			// only remount if unique mount options are set
			if err := remount(m, rootfs); err != nil {
				return err
			}
		}

		// if m.Relabel != "" {
		//  if err := label.Validate(m.Relabel); err != nil {
		//      return err
		//  }
		//  shared := label.IsShared(m.Relabel)
		//  if err := label.Relabel(m.Source, mountLabel, shared); err != nil {
		//      return err
		//  }
		// }
	// case "cgroup":
	//  binds, err := getCgroupMounts(m)
	//  if err != nil {
	//      return err
	//  }
	//  var merged []string
	//  for _, b := range binds {
	//      ss := filepath.Base(b.Destination)
	//      if strings.Contains(ss, ",") {
	//          merged = append(merged, ss)
	//      }
	//  }
	//  tmpfs := &configs.Mount{
	//      Source:           "tmpfs",
	//      Device:           "tmpfs",
	//      Destination:      m.Destination,
	//      Flags:            defaultMountFlags,
	//      Data:             "mode=755",
	//      PropagationFlags: m.PropagationFlags,
	//  }
	//  if err := mountToRootfs(tmpfs, rootfs, mountLabel); err != nil {
	//      return err
	//  }
	//  for _, b := range binds {
	//      if err := mountToRootfs(b, rootfs, mountLabel); err != nil {
	//          return err
	//      }
	//  }
	//  for _, mc := range merged {
	//      for _, ss := range strings.Split(mc, ",") {
	//          // symlink(2) is very dumb, it will just shove the path into
	//          // the link and doesn't do any checks or relative path
	//          // conversion. Also, don't error out if the cgroup already exists.
	//          if err := os.Symlink(mc, filepath.Join(rootfs, m.Destination, ss)); err != nil && !os.IsExist(err) {
	//              return err
	//          }
	//      }
	//  }
	//  if m.Flags&unix.MS_RDONLY != 0 {
	//      // remount cgroup root as readonly
	//      mcgrouproot := &configs.Mount{
	//          Source:      m.Destination,
	//          Device:      "bind",
	//          Destination: m.Destination,
	//          Flags:       defaultMountFlags | unix.MS_RDONLY | unix.MS_BIND,
	//      }
	//      if err := remount(mcgrouproot, rootfs); err != nil {
	//          return err
	//      }
	//  }
	default:
		// ensure that the destination of the mount is resolved of symlinks at mount time because
		// any previous mounts can invalidate the next mount's destination.
		// this can happen when a user specifies mounts within other mounts to cause breakouts or other
		// evil stuff to try to escape the container's rootfs.
		var err error
		if dest, err = FollowSymlinkInScope(dest, rootfs); err != nil {
			return err
		}
		if err := checkMountDestination(rootfs, dest); err != nil {
			return err
		}
		// update the mount with the correct dest after symlinks are resolved.
		m.Destination = dest
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		return mountPropagate(m, rootfs, mountLabel)
	}
	return nil
}

func remount(m *configs.Mount, rootfs string) error {
	var (
		dest = m.Destination
	)
	if !strings.HasPrefix(dest, rootfs) {
		dest = filepath.Join(rootfs, dest)
	}
	if err := unix.Mount(m.Source, dest, m.Device, uintptr(m.Flags|unix.MS_REMOUNT), ""); err != nil {
		return err
	}
	return nil
}

func getMountInfo(mountinfo []*mount.Info, dir string) *mount.Info {
	for _, m := range mountinfo {
		if m.Mountpoint == dir {
			return m
		}
	}
	return nil
}

// Get the parent mount point of directory passed in as argument. Also return
// optional fields.
func getParentMount(rootfs string) (string, string, error) {
	var path string

	mountinfos, err := mount.GetMounts()
	if err != nil {
		return "", "", err
	}

	mountinfo := getMountInfo(mountinfos, rootfs)
	if mountinfo != nil {
		return rootfs, mountinfo.Optional, nil
	}

	path = rootfs
	for {
		path = filepath.Dir(path)

		mountinfo = getMountInfo(mountinfos, path)
		if mountinfo != nil {
			return path, mountinfo.Optional, nil
		}

		if path == "/" {
			break
		}
	}

	// If we are here, we did not find parent mount. Something is wrong.
	return "", "", fmt.Errorf("Could not find parent mount of %s", rootfs)
}

// Make parent mount private if it was shared
func rootfsParentMountPrivate(rootfs string) error {
	sharedMount := false

	parentMount, optionalOpts, err := getParentMount(rootfs)
	if err != nil {
		return err
	}

	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			sharedMount = true
			break
		}
	}

	// Make parent mount PRIVATE if it was shared. It is needed for two
	// reasons. First of all pivot_root() will fail if parent mount is
	// shared. Secondly when we bind mount rootfs it will propagate to
	// parent namespace and we don't want that to happen.
	if sharedMount {
		return unix.Mount("", parentMount, "", unix.MS_PRIVATE, "")
	}

	return nil
}
