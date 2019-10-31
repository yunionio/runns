package main

import (
	"fmt"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func msMoveRoot(rootfs string) error {
	if err := unix.Mount(rootfs, "/", "", unix.MS_MOVE, ""); err != nil {
		return err
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	return unix.Chdir("/")
}

// pivotRoot will call pivot_root such that rootfs becomes the new root
// filesystem, and everything else is cleaned up.
func pivotRoot(rootfs string) error {
	// While the documentation may claim otherwise, pivot_root(".", ".") is
	// actually valid. What this results in is / being the new root but
	// /proc/self/cwd being the old root. Since we can play around with the cwd
	// with pivot_root this allows us to pivot without creating directories in
	// the rootfs. Shout-outs to the LXC developers for giving us this idea.

	oldroot, err := unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(oldroot)

	newroot, err := unix.Open(rootfs, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(newroot)

	// Change to the new root so that the pivot_root actually acts on it.
	if err := unix.Fchdir(newroot); err != nil {
		return err
	}

	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root %s", err)
	}

	// Currently our "." is oldroot (according to the current kernel code).
	// However, purely for safety, we will fchdir(oldroot) since there isn't
	// really any guarantee from the kernel what /proc/self/cwd will be after a
	// pivot_root(2).

	if err := unix.Fchdir(oldroot); err != nil {
		return err
	}

	// Make oldroot rprivate to make sure our unmounts don't propagate to the
	// host (and thus bork the machine).
	if err := unix.Mount("", ".", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return err
	}
	// Preform the unmount. MNT_DETACH allows us to unmount /proc/self/cwd.
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return err
	}

	// Switch back to our shiny new root.
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir / %s", err)
	}
	return nil
}

func prepareRoot(config *configs.Config) error {
	flag := unix.MS_SLAVE | unix.MS_REC
	if config.RootPropagation != 0 {
		flag = config.RootPropagation
	}
	if err := unix.Mount("", "/", "", uintptr(flag), ""); err != nil {
		return errors.Wrap(err, "first prepare root")
	}

	// Make parent mount private to make sure following bind mount does
	// not propagate in other namespaces. Also it will help with kernel
	// check pass in pivot_root. (IS_SHARED(new_mnt->mnt_parent))
	if err := rootfsParentMountPrivate(config.Rootfs); err != nil {
		return errors.Wrap(err, "rootfs parent mount private")
	}

	if err := unix.Mount(config.Rootfs, config.Rootfs, "bind", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return errors.Wrap(err, "bind mount rootfs")
	}
	return nil
}

func prepareRootfs(config *configs.Config) error {
	if err := prepareRoot(config); err != nil {
		return errors.Wrap(err, "preparing rootfs")
	}

	for _, m := range config.Mounts {
		// for _, precmd := range m.PremountCmds {
		//  if err := mountCmd(precmd); err != nil {
		//      return errors.Wrap(err, "running premount command")
		//  }
		// }

		if err := mountToRootfs(m, config.Rootfs, config.MountLabel); err != nil {
			return errors.Wrapf(err, "mounting %q to rootfs %q at %q", m.Source, config.Rootfs, m.Destination)
		}

		// for _, postcmd := range m.PostmountCmds {
		//  if err := mountCmd(postcmd); err != nil {
		//      return errors.Wrap(err, "running postmount command")
		//  }
		// }
	}

	// The reason these operations are done here rather than in finalizeRootfs
	// is because the console-handling code gets quite sticky if we have to set
	// up the console before doing the pivot_root(2). This is because the
	// Console API has to also work with the ExecIn case, which means that the
	// API must be able to deal with being inside as well as outside the
	// container. It's just cleaner to do this here (at the expense of the
	// operation not being perfectly split).

	if err := unix.Chdir(config.Rootfs); err != nil {
		return errors.Wrapf(err, "changing dir to %q", config.Rootfs)
	}
	//difference with pivot root and ms_move root
	var err error
	if config.NoPivotRoot {
		err = msMoveRoot(config.Rootfs)
	} else {
		err = pivotRoot(config.Rootfs)
	}
	if err != nil {
		return fmt.Errorf("jailing process inside rootfs: %s", err)
	}
	return nil
}
