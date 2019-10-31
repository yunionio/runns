package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strconv"
	"syscall"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

var specConfig = "config.json"
var listPath = "/run/runns"

func ncExist(ncName string) (bool, error) {
	files, err := ioutil.ReadDir(listPath)
	if err != nil {
		return false, errors.Wrap(err, "read dir")
	}
	for _, f := range files {
		if f.Name() == ncName {
			return true, nil
		}
	}
	return false, nil
}

func validateNcName() (string, error) {
	if len(os.Args) < 3 {
		return "", errors.New("missing args ...")
	}
	ncName := os.Args[2]
	exist, err := ncExist(ncName)
	if err != nil {
		return "", err
	} else if exist {
		return "", errors.Errorf("container %s exist", ncName)
	}
	return ncName, nil
}

func main() {
	if len(os.Args) == 1 {
		panic("missing cmdline input")
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run()
	case "child":
		err = child()
	case "kill":
		err = kill()
	case "list":
		err = list()
	default:
		panic("unknonw input")
	}
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func kill() error {
	if len(os.Args) < 3 {
		return errors.New("kill missing target?")
	}
	ncName := os.Args[2]
	if exist, err := ncExist(ncName); err != nil {
		return err
	} else if !exist {
		return errors.Errorf("container %s not exist", ncName)
	}
	pidStr, err := FileGetContents(path.Join(listPath, ncName))
	if err != nil {
		return errors.Wrap(err, "fetch container failed")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return errors.Wrap(err, "convert pid to int failed")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return errors.Wrap(err, "find process")
	}
	err = os.Remove(path.Join(listPath, ncName))
	if err != nil {
		return errors.Wrap(err, "remove pid file")
	}
	err = p.Kill()
	if err != nil {
		return errors.Wrap(err, "kill process")
	}
	return nil
}

func list() error {
	files, err := ioutil.ReadDir(listPath)
	if err != nil {
		return errors.Wrap(err, "read dir")
	}
	var prints string
	for _, f := range files {
		if f.Mode().IsRegular() {
			name := f.Name()
			pidStr, err := FileGetContents(path.Join(listPath, name))
			if err != nil {
				return errors.Wrap(err, "read list path files")
			}
			prints += fmt.Sprintf("%s %s\n", name, pidStr)
		}
	}
	print(prints)
	return nil
}

func run() error {
	_, err := os.Lstat(listPath)
	if err != nil {
		err = os.MkdirAll(listPath, os.ModePerm)
		if err != nil {
			return errors.Wrap(err, "mkdir for list path")
		}
	}
	ncName, err := validateNcName()
	if err != nil {
		return err
	}

	spec, err := initSpec(specConfig)
	if err != nil {
		return err
	}
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, os.Args[2:]...)...)
	cmd.SysProcAttr = &unix.SysProcAttr{
		// Cloneflags: unix.CLONE_NEWUTS | unix.CLONE_NEWPID | unix.CLONE_NEWNS,
		Cloneflags: unix.CLONE_NEWNS | unix.CLONE_NEWPID,
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// cmd.Dir = spec.Root.Path

	specBytes, err := json.Marshal(spec)
	if err != nil {
		return errors.Wrap(err, "marshal spec")
	}
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("_LIBCONTAINER_SPEC=%s", specBytes),
		// fmt.Sprintf("_LIBCONTAINER_NCNAME=%s", ncName),
	)

	// start replase run for detach mode
	if err := cmd.Start(); err != nil {
		return errors.Wrap(err, "start child")
	}
	err = FilePutContents(path.Join(listPath, ncName), strconv.Itoa(cmd.Process.Pid), false)
	if err != nil {
		return errors.Wrap(err, "put parent pid")
	}
	return nil
}

func child() error {
	//setsid
	sid, err := unix.Setsid()
	if err != nil {
		return errors.Wrap(err, "child setsid")
	}
	if sid == -1 {
		return errors.New("child setsid is -1")
	}

	_, err = os.Lstat(listPath)
	if err != nil {
		err = os.MkdirAll(listPath, os.ModePerm)
		if err != nil {
			return errors.Wrap(err, "mkdir for list path")
		}
	}
	// ncName := os.Getenv("_LIBCONTAINER_NCNAME")
	// if len(ncName) == 0 {
	// 	return errors.New("missing ncname")
	// }
	return initRun()
}

// run in child process
func initRun() error {
	var spec = new(specs.Spec)
	specStr := os.Getenv("_LIBCONTAINER_SPEC")
	err := json.Unmarshal([]byte(specStr), spec)
	if err != nil {
		return errors.Wrap(err, "unmarshal spec")
	}
	config, err := prepareConfig(spec)
	if err != nil {
		return errors.Wrap(err, "prepare config")
	}
	err = prepareRootfs(config)
	if err != nil {
		return errors.Wrap(err, "prepare rootfs")
	}
	name, err := exec.LookPath(spec.Process.Args[0])
	if err != nil {
		return errors.Wrap(err, "look path")
	}
	if err := syscall.Exec(name, spec.Process.Args[0:], os.Environ()); err != nil {
		return errors.Wrap(err, "exec user process")
	}
	return nil
}
