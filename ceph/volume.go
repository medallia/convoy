package ceph

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	// "strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/rancher/convoy/util/fs"
	. "github.com/rancher/convoy/convoydriver"
)

type Volume struct {
	m sync.Mutex
	// unique name of the volume
	Name string
	// the path to the device to which the Ceph volume has been mapped
	Device string
	// the path to the LUKS folder to which the Ceph device has been mapped
	LUKSDevice string
	// Host path
	MountPoint string
}

/* Caller must implement VolumeHelper interface, and must have fields "Name" and "MountPoint" */
type VolumeHelper interface {
	GetDevice() (string, error)
	GetMountOpts() []string
	GenerateDefaultMountPoint() string
}

func (v *Volume) GetDevice() (string, error) {
	// If there is a LUKS device then return that else return base device
	if v.LUKSDevice != "" {
		return v.LUKSDevice, nil
	}
	return v.Device, nil
}

func (v *Volume) GetMountOpts() []string {
	return []string{}
}

func (v *Volume) GenerateDefaultMountPoint() string {
	return filepath.Join("/var/lib/rancher/convoy/ceph/mounts", v.Name)
}
const (
	CephImageSizeMB   = 512 // 1TB
	LuksDevMapperPath = "/dev/mapper/"
	cryptoLuksFsType  = "crypto_LUKS"
)

func (v *Volume) Info() map[string]string {
	return map[string]string {
		OPT_VOLUME_NAME: v.Name,
		OPT_MOUNT_POINT: v.MountPoint,
		"Device": v.Device,
		"LuksDevice": v.LUKSDevice,
	}
}

func (v *Volume) mapCephVolume() (error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command("rbd", "map", v.Name)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	var Device string
	if err := cmd.Run(); err == nil {
		Device = strings.TrimRight(stdout.String(), "\n")
		log.Infof("Succeeded in mapping Ceph volume '%s' to %s", v.Name, Device)
		v.Device = Device
		return nil
	} else {
		msg := fmt.Sprintf("Failed to map Ceph volume '%s': %s - %s", v.Name, err, strings.TrimRight(stderr.String(), "\n"))
		log.Errorf(msg)
		return errors.New(msg)
	}
}

func (v *Volume) unmapCephVolume() error {
	cmd := exec.Command("rbd", "unmap", v.Device)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		log.Infof("Succeeded in unmapping Ceph volume '%s' from %s", v.Name, v.Device)
		v.Device = ""
	} else {
		log.Errorf("Failed to unmap Ceph volume '%s' from %s: %s - %s", v.Name, v.Device, err, strings.TrimRight(stderr.String(), "\n"))
	}
	return err
}

func (v *Volume) Map(id string, sizeMB int64) (Device string, returnedError error) {
	v.m.Lock()
	defer v.m.Unlock()
	log.Debugf("Map: %s", Device)
	// The RBD wrapper is the gatekeeper so no need to keep track of the usage

	// TODO: Might want to map with --options rw/ro here, but then we need to sneak in the RW flag somehow
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	cmd := exec.Command("rbd", "create", v.Name, "--size", fmt.Sprintf("%v", sizeMB))
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		log.Infof("Created Ceph volume '%s'", v.Name)
	} else {
		// if rbd create returned EEXIST (17) the image is already there and we just need to map
		if exitError, ok := err.(*exec.ExitError); ok {
			imageSpec := strings.Split(v.Name, "/") // strip the pool from the name
			imageName := imageSpec[len(imageSpec) - 1]
			waitStatus := exitError.Sys().(syscall.WaitStatus)
			if waitStatus.ExitStatus() == 17 || strings.Contains(stderr.String(), fmt.Sprintf("rbd image %s already exists", imageName)) {
				log.Infof("Found existing Ceph volume '%s'. %s", v.Name, err)
			} else {
				msg := fmt.Sprintf("Failed to create Ceph volume '%s'. %s. (%d) ", v.Name, stderr.String(), waitStatus.ExitStatus())
				log.Errorf(msg)
				return "", errors.New(msg)
			}
		} else {
			log.Errorf(fmt.Sprintf("Failed to get exit code from ceph volume creation '%s'. %s", v.Name, stderr.String()))
			return "", err
		}
	}
	if err := v.mapCephVolume(); err != nil {
		return "", err
	}
	return v.Device, nil
}

func (v *Volume) LUKSEncyption() error {
	luksDevMapperName := getLuksDeviceMapperName(v.Name)
	cmd := exec.Command("cryptsetup", "luksOpen", "--allow-discards", "--key-file=-", v.Device, luksDevMapperName)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Errorf("Failed to luksOpen Ceph volume '%s' (device %s) - %s", v.Name, v.Device, err)
		return err
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		log.Errorf("Failed to luksOpen Ceph volume '%s' (device %s) - %s", v.Name, v.Device, err)
		return err
	}

	key, err := getLuksKey(v.Name)
	if err != nil {
		log.Errorf("Failed to luksOpen Ceph volume '%s' (device %s) - %s", v.Name, v.Device, err)
		return err
	}

	io.WriteString(stdin, key)
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		log.Errorf("Failed to luksOpen Ceph volume '%s' (device %s) - %s", v.Name, v.Device, err)
		return err
	}

	v.LUKSDevice = path.Join(LuksDevMapperPath, luksDevMapperName)
	fsType, err :=  fs.Detect(v.LUKSDevice)
	log.Errorf("Filesystem in %s: %s", v.LUKSDevice, fsType)
	if err != nil {
		return err
	}
	return nil
}

func (v *Volume) detectFS() (string, error) {
	log.Debugf("Trying to detect fs on '%v'", v.Device)
	fsType, err := fs.Detect(v.Device)
	if err == nil {
		log.Debugf("Detected '%v' filesystem on '%v'", fsType, v.Device)
	}
	return fsType, err
}

func (v *Volume) createFS() error {
	cmd := exec.Command("mkfs.ext4", "-m0", "-E", "nodiscard,lazy_itable_init=0,lazy_journal_init=0,packed_meta_blocks=1", v.Device)
	log.Infof("Creating ext4 filesystem in newly created Ceph volume '%s' (device %s)", v.Name, v.Device)
	if err := cmd.Run(); err != nil {
		log.Errorf("Failed to create ext4 filesystem in newly created Ceph volume '%s' (device %s) - %s", v.Name, v.Device, err)
		return err
	}
	return nil
}

func (v *Volume) resizeFS() error {
	log.Debugf("Ensuring filesystem size and device=%v size match", v.Device)
	if err := fs.Resize(v.Device); err != nil {
		log.Debugf("Syncing device=%s sizes error: %s", v.Device, err)
		return err
	}
	return nil;
}

func (v *Volume) checkFS() error {
	fsckCmd, err := exec.Command("fsck", "-a", v.Device).Output()
	if err != nil {
		log.Errorf("Failed to check filesystem in %s - %s", v.Name, err)
		return err
	}
	log.Infof("Checked filesystem in %s: %s", v.Name, fsckCmd)
	return nil
}

func (v *Volume) Unmount(id string) error {
	v.m.Lock()
	defer v.m.Unlock()
	defer v.unmapCephVolume()
	fsType, err := fs.Detect(v.Device)
	if err != nil {
		return err
	}

	if fsType == cryptoLuksFsType {
		luksDevMapperName := getLuksDeviceMapperName(v.Name)
		cmd := exec.Command("cryptsetup", "luksClose", luksDevMapperName)
		if err := cmd.Run(); err != nil {
			log.Errorf("Failed to luksClose Ceph volume '%s' (device %s) - %s", v.Name, v.Device, err)
			return err
		}
	}
	return nil
}

func getLuksKey(name string) (string, error) {
	return name, nil
}

func getLuksDeviceMapperName(name string) (string) {
	return strings.Replace(name, "/", "--", -1)
}