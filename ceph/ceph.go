package ceph

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"sync"

	"github.com/Sirupsen/logrus"

	. "github.com/rancher/convoy/convoydriver"
	"github.com/rancher/convoy/util"
	"github.com/rancher/convoy/util/fs"
)

var (
	log = logrus.WithFields(logrus.Fields{"pkg": "ceph"})
)

const (
	driverName = "ceph"

	cephDefaultVolumeSize = "ceph.defaultvolumesize"
	cephDefaultEncrypted  = "ceph.defaultencrypted"

	cephDefaultFSType = "ceph.defaultfilesystem"
	cephDefaultFSOptions = "ceph.defaultfilesystemoptions"

	defaultVolumeSize = "10G"
	defaultEncrypted  = "false" // Currently unused, but may be supported in future

	defaultFSType     = "ext4"
	defaultFSOptions  = ""
)

type Driver struct {
	mutex   *sync.RWMutex
	volumes map[string]*Volume
	*Device
}

type Device struct {
	Root              string
	DefaultVolumeSize int64
	DefaultEncrypted  bool
	DefaultFSType     string
	DefaultFSOptions  string
}

func (d *Driver) VolumeOps() (VolumeOperations, error) {
	return d, nil
}

func (Driver) SnapshotOps() (SnapshotOperations, error) {
	return nil, fmt.Errorf("Snapshot ops not supported")
}

func (Driver) BackupOps() (BackupOperations, error) {
	return nil, fmt.Errorf("Backup ops not supported")
}

func (d *Driver) Info() (map[string]string, error) {
	return map[string]string{
		"name": d.Name(),
	}, nil
}

func init() {
	if err := Register(driverName, Init); err != nil {
		panic(err)
	}
}

func (*Driver) Name() string {
	return "ceph"
}

// CreateVolume is empty as we create on mount
func (d *Driver) CreateVolume(req Request) error {
	return nil
}

// createVolume is used on mount to generate the volumes internal state
func (d *Driver) createVolume(req Request) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	v, exists := d.volumes[req.Name]
	if !exists {
		v = &Volume{
			Name:             req.Name,
			Device:           "", // Will be set by Mount()
			LUKSDevice:       "", // Will be set by Mount()
			MountPointPrefix: d.Root,
		}
		d.volumes[req.Name] = v
	}
}

func (d *Driver) DeleteVolume(req Request) error {
	currentImageMap, err := d.getCurrentVolumes()
	if err != nil {
		return err
	}
	// if the image is still mapped then don't delete the volume from Convoy internal state
	if _, exists := currentImageMap[req.Name]; exists {
		return nil
	}
	if _, exists := d.volumes[req.Name]; exists {
		delete(d.volumes, req.Name)
	}
	return nil
}

func (d *Driver) checkDevice(device string) error {
	_, err := fs.Detect(device)
	if err == fs.ErrNoFilesystemDetected {
		if err = fs.FormatDevice(device, d.DefaultFSType, d.DefaultFSOptions); err != nil {
			return err
		}
		log.Debugf("Formatted device=%v with fs=%v and options=%v",device, d.DefaultFSType, d.DefaultFSOptions)
	} else if err != nil {
		return err
	}
	// Resizing of LUKS is not currently supported
	if err = fs.Resize(device); err != nil {
		return err
	}
	log.Debugf("Resized device=%v if necessary", device)
	if err = fs.Check(device); err != nil {
		return err
	}
	log.Debugf("Checked FS integrity on device=%v", device)
	return nil
}

func (d *Driver) MountVolume(req Request) (string, error) {
	// if the volume state does not exist then generate it
	if _, exists := d.volumes[req.Name]; !exists {
		d.createVolume(req)
	}
	volume := d.volumes[req.Name]
	// Map the volume
	var err error
	if _, err := volume.Map(req.Name, d.DefaultVolumeSize); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			if err = volume.unmapCephVolume(); err != nil {
				log.Debug(err)
			}
		}
	}()

	var mountDevice string // Could change due to LUKS
	fsType, err := fs.Detect(volume.Device)
	if err != nil && err != fs.ErrNoFilesystemDetected {
		return "", err
	}
	if fsType == cryptoLuksFsType {
		if err = volume.LUKSEncryption(); err != nil {
			return "", err
		}
		mountDevice = volume.LUKSDevice
	} else {
		mountDevice = volume.Device
	}
	if err = d.checkDevice(mountDevice); err != nil {
		return "", err
	}
	// Mount the volume
	mountPoint, err := util.VolumeMount(volume, "", false)
	return mountPoint, err
}

func (d *Driver) UmountVolume(req Request) error {
	volume, exists := d.volumes[req.Name]
	if !exists {
		return fmt.Errorf("Failed Unmount because %v does not exist in internal state", req.Name)
	}
	// Umount the volume
	if err := util.VolumeUmount(volume); err != nil {
		return err
	}
	// Unmap the volume
	if err := volume.Unmap(req.Name); err != nil {
		return err
	}
	return nil
}

func (d *Driver) MountPoint(req Request) (string, error) {
	volume, exists := d.volumes[req.Name]
	if !exists {
		return "", fmt.Errorf("Volume=%v is not mounted", req.Name)
	}
	return volume.MountPoint, nil
}

type RBDShowmapped struct {
	Pool   string
	Name   string
	Snap   string
	Device string
}

// getCurrentVolumes gets all volumes that are mapped
func (d *Driver) getCurrentVolumes() (map[string]interface{}, error) {
	cmd := exec.Command("rbd", "showmapped", "--format=json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	mappedVolumes := make(map[string]RBDShowmapped)
	if err := json.Unmarshal(output, &mappedVolumes); err != nil {
		return nil, err
	}
	imageMap := make(map[string]interface{})
	for _, image := range mappedVolumes {
		imageMap[image.Name] = nil
	}
	return imageMap, nil
}

func (d *Driver) GetVolumeInfo(name string) (map[string]string, error) {
	volume, exists := d.volumes[name]
	if !exists {
		return nil, util.ErrorNotExists()
	}
	return volume.Info(), nil
}

func (d *Driver) ListVolume(opts map[string]string) (map[string]map[string]string, error) {
	listVolumeMap := make(map[string]map[string]string)
	for volumeName, volume := range d.volumes {
		listVolumeMap[volumeName] = volume.Info()
	}
	return listVolumeMap, nil
}

func Init(root string, config map[string]string) (ConvoyDriver, error) {
	device, err := getDefaultDevice(root, config)
	if err != nil {
		return nil, err
	}
	d := &Driver{
		mutex:   &sync.RWMutex{},
		volumes: make(map[string]*Volume),
		Device:  device,
	}
	return d, nil
}

func getDefaultDevice(root string, config map[string]string) (*Device, error) {
	if config[cephDefaultEncrypted] == "" {
		config[cephDefaultEncrypted] = defaultEncrypted
	}
	if config[cephDefaultVolumeSize] == "" {
		config[cephDefaultVolumeSize] = defaultVolumeSize
	}
	if config[cephDefaultFSType] == "" {
		config[cephDefaultFSType] = defaultFSType
	}
	if config[cephDefaultFSOptions] == "" {
		config[cephDefaultFSOptions] = defaultFSOptions
	}
	size, err := util.ParseSize(config[cephDefaultVolumeSize])
	if err != nil {
		return nil, err
	}
	size = size / (1024 * 1024) // Convert from bytes to megabytes
	encrypted, err := strconv.ParseBool(config[cephDefaultEncrypted])
	if err != nil {
		return nil, err
	}

	dev := &Device{
		DefaultVolumeSize: size,
		DefaultEncrypted:  encrypted,
		DefaultFSType: config[cephDefaultFSType],
		DefaultFSOptions: config[cephDefaultFSOptions],
		Root:              root,
	}
	return dev, nil
}
