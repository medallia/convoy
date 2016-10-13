package util

import (
	"fmt"
	"os"
	"reflect"
	"strings"
)

const (
	MOUNT_BINARY   = "mount"
	UMOUNT_BINARY  = "umount"
	NSENTER_BINARY = "nsenter"
)

var (
	mountNamespaceFD = ""
)

/* Caller must implement VolumeHelper interface, and must have fields "UUID" and "MountPoint" */
type VolumeHelper interface {
	GetDevice() (string, error)
	GetMountOpts() []string
	GenerateDefaultMountPoint() string
}

func getFieldString(obj interface{}, field string) (string, error) {
	if reflect.TypeOf(obj).Kind() != reflect.Ptr {
		return "", fmt.Errorf("BUG: Non-pointer was passed in")
	}
	t := reflect.TypeOf(obj).Elem()
	if _, found := t.FieldByName(field); !found {
		return "", fmt.Errorf("BUG: %v doesn't have required field %v", t, field)
	}
	return reflect.ValueOf(obj).Elem().FieldByName(field).String(), nil
}

func setFieldString(obj interface{}, field string, value string) error {
	if reflect.TypeOf(obj).Kind() != reflect.Ptr {
		return fmt.Errorf("BUG: Non-pointer was passed in")
	}
	t := reflect.TypeOf(obj).Elem()
	if _, found := t.FieldByName(field); !found {
		return fmt.Errorf("BUG: %v doesn't have required field %v", t, field)
	}
	v := reflect.ValueOf(obj).Elem().FieldByName(field)
	if !v.CanSet() {
		return fmt.Errorf("BUG: %v doesn't have setable field %v", t, field)
	}
	v.SetString(value)
	return nil
}

func getVolumeUUID(v VolumeHelper) string {
	// We should already pass the test in getVolumeOps
	value, err := getFieldString(v, "UUID")
	if err != nil {
		panic(err)
	}
	return value
}

func getVolumeMountPoint(v VolumeHelper) string {
	// We should already pass the test in getVolumeOps
	value, err := getFieldString(v, "MountPoint")
	if err != nil {
		panic(err)
	}
	return value
}

func setVolumeMountPoint(v VolumeHelper, value string) {
	// We should already pass the test in getVolumeOps
	if err := setFieldString(v, "MountPoint", value); err != nil {
		panic(err)
	}
}

func getVolumeOps(obj interface{}) (VolumeHelper, error) {
	var err error
	if reflect.TypeOf(obj).Kind() != reflect.Ptr {
		return nil, fmt.Errorf("BUG: Non-pointer was passed in")
	}
	_, err = getFieldString(obj, "UUID")
	if err != nil {
		return nil, err
	}
	mountpoint, err := getFieldString(obj, "MountPoint")
	if err != nil {
		return nil, err
	}
	if err = setFieldString(obj, "MountPoint", mountpoint); err != nil {
		return nil, err
	}
	t := reflect.TypeOf(obj).Elem()
	ops, ok := obj.(VolumeHelper)
	if !ok {
		return nil, fmt.Errorf("BUG: %v doesn't implement necessary methods for accessing volume", t)
	}
	return ops, nil
}

func isMounted(dev, mountPoint string) bool {
	output, err := callMount([]string{}, []string{})
	if err != nil {
		return false
	}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, dev) && strings.Contains(line, mountPoint) {
			return true
		}
	}
	return false
}

func VolumeMount(v interface{}, mountPoint string) (string, error) {
	vol, err := getVolumeOps(v)
	if err != nil {
		return "", err
	}
	dev, err := vol.GetDevice()
	if err != nil {
		return "", err
	}
	opts := vol.GetMountOpts()
	if mountPoint == "" {
		mountPoint = vol.GenerateDefaultMountPoint()
		if err := MkdirIfNotExists(mountPoint); err != nil {
			return "", err
		}
	}
	if st, err := os.Stat(mountPoint); err != nil || !st.IsDir() {
		return "", fmt.Errorf("Specified mount point %v is not a directory", mountPoint)
	}
	existMount := getVolumeMountPoint(vol)
	if existMount != "" && existMount != mountPoint {
		return "", fmt.Errorf("Volume %v was already mounted at %v, but asked to mount at %v", getVolumeUUID(vol), existMount, mountPoint)
	}
	if !isMounted(dev, mountPoint) {
		log.Debugf("Volume %v is not mounted, mount it now to %v, with option %v", getVolumeUUID(vol), mountPoint, opts)
		_, err = callMount(opts, []string{dev, mountPoint})
		if err != nil {
			return "", err
		}
	}
	setVolumeMountPoint(vol, mountPoint)
	return mountPoint, nil
}

func VolumeUmount(v interface{}) error {
	vol, err := getVolumeOps(v)
	if err != nil {
		return err
	}
	mountPoint := getVolumeMountPoint(vol)
	if mountPoint == "" {
		log.Debugf("Umount a umounted volume %v", getVolumeUUID(vol))
		return nil
	}
	if err := callUmount([]string{mountPoint}); err != nil {
		return err
	}
	if mountPoint == vol.GenerateDefaultMountPoint() {
		if err := os.Remove(mountPoint); err != nil {
			log.Warnf("Cannot cleanup mount point directory %v due to %v\n", mountPoint, err)
		}
	}
	setVolumeMountPoint(vol, "")
	return nil
}

func callMount(opts, args []string) (string, error) {
	cmdName := MOUNT_BINARY
	cmdArgs := opts
	cmdArgs = append(cmdArgs, args...)
	if mountNamespaceFD != "" {
		newArgs := []string{
			"--mount=" + mountNamespaceFD,
			cmdName,
		}
		cmdArgs = append(newArgs, cmdArgs...)
		cmdName = NSENTER_BINARY
		log.Debugf("Mount in namespace %v", mountNamespaceFD)
	}
	output, err := Execute(cmdName, cmdArgs)
	if err != nil {
		return "", err
	}
	return output, nil
}

func callUmount(args []string) error {
	cmdName := UMOUNT_BINARY
	cmdArgs := args
	if mountNamespaceFD != "" {
		cmdArgs = []string{
			"--mount=" + mountNamespaceFD,
			cmdName,
		}
		cmdArgs = append(cmdArgs, args...)
		cmdName = NSENTER_BINARY
		log.Debugf("Umount in namespace %v", mountNamespaceFD)
	}
	if _, err := Execute(cmdName, cmdArgs); err != nil {
		return err
	}
	return nil
}

func InitMountNamespace(fd string) error {
	if fd == "" {
		return nil
	}
	if _, err := Execute(NSENTER_BINARY, []string{"-V"}); err != nil {
		return fmt.Errorf("Cannot find nsenter for namespace switching")
	}
	if _, err := Execute(NSENTER_BINARY, []string{"--mount=" + fd, "mount"}); err != nil {
		return fmt.Errorf("Invalid mount namespace %v, error %v", fd, err)
	}

	mountNamespaceFD = fd
	return nil
}
