package statemachine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type mountPoint struct {
	src     string
	relpath string
	path    string
	typ     string
	opts    []string
	bind    bool
}

// getMountCmd returns mount/umount commands to mount the given mountpoint
// If the mountpoint does not exist, it will be created.
func getMountCmd(typ string, src string, targetDir string, mountpoint string, bind bool, options ...string) (mountCmds, umountCmds []*exec.Cmd, err error) {
	if bind && len(typ) > 0 {
		return nil, nil, fmt.Errorf("invalid mount arguments. Cannot use --bind and -t at the same time.")
	}

	targetPath := filepath.Join(targetDir, mountpoint)
	mountCmd := execCommand("mount")

	if len(typ) > 0 {
		mountCmd.Args = append(mountCmd.Args, "-t", typ)
	}

	if bind {
		mountCmd.Args = append(mountCmd.Args, "--bind")
	}

	mountCmd.Args = append(mountCmd.Args, src)
	if len(options) > 0 {
		mountCmd.Args = append(mountCmd.Args, "-o", strings.Join(options, ","))
	}
	mountCmd.Args = append(mountCmd.Args, targetPath)

	if _, err := os.Stat(targetPath); err != nil {
		err := osMkdirAll(targetPath, 0755)
		if err != nil && !os.IsExist(err) {
			return nil, nil, fmt.Errorf("Error creating mountpoint \"%s\": \"%s\"", targetPath, err.Error())
		}
	}

	umountCmds = getUnmountCmd(targetPath)

	return []*exec.Cmd{mountCmd}, umountCmds, nil
}

// getUnmountCmd generates unmount commands from a path
func getUnmountCmd(targetPath string) []*exec.Cmd {
	return []*exec.Cmd{
		execCommand("mount", "--make-rprivate", targetPath),
		execCommand("umount", "--recursive", targetPath),
	}
}

// diffMountPoints compares 2 lists of mountpoint and returns the added ones
func diffMountPoints(olds []mountPoint, currents []mountPoint) (added []mountPoint) {
	for _, m := range currents {
		found := false
		for _, o := range olds {
			if m.src == o.src {
				found = true
			}
		}
		if !found {
			added = append(added, m)
		}
	}

	return added
}

// listMounts returns mountpoints matching the given path from /proc/self/mounts
func listMounts(path string) ([]mountPoint, error) {
	procMounts := "/proc/self/mounts"
	f, err := osReadFile(procMounts)
	if err != nil {
		return nil, err
	}

	return parseMounts(string(f), path)
}

// parseMounts list existing mounts and submounts in the current path
// The returned splice is already inverted so unmount can be called on it
// without further modification.
func parseMounts(procMount string, path string) ([]mountPoint, error) {
	mountPoints := []mountPoint{}
	mountLines := strings.Split(procMount, "\n")

	for _, line := range mountLines {
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		mountPath := fields[1]

		if len(path) != 0 && !strings.HasPrefix(mountPath, path) {
			continue
		}

		m := mountPoint{
			src:  fields[0],
			path: mountPath,
			typ:  fields[2],
			opts: strings.Split(fields[3], ","),
		}
		mountPoints = append([]mountPoint{m}, mountPoints...)
	}

	return mountPoints, nil
}
